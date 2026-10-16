package app

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxRoomNameRunes   = 48
	maxMemberNameRunes = 32
	maxChatRunes       = 2000
	maxChatHistory     = 100
	passwordIterations = 120000
)

var (
	errInvalidName     = errors.New("name is empty or too long")
	errInvalidRoomID   = errors.New("room id must be 4-32 ASCII letters, digits, hyphens or underscores")
	errInvalidPassword = errors.New("room password must contain 6-128 characters without control characters")
	errRoomExists      = errors.New("room id already exists")
	errRoomNotFound    = errors.New("room not found or password is invalid")
	errInvalidToken    = errors.New("invalid room member token")
	errNotConnected    = errors.New("websocket connection is no longer active")
	errInvalidChat     = errors.New("chat message is empty or too long")
)

type Member struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	JoinedAt time.Time `json:"joined_at"`
}

type ChatMessage struct {
	ID        string    `json:"id"`
	MemberID  string    `json:"member_id"`
	Name      string    `json:"name"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

type JoinResult struct {
	RoomID      string   `json:"room_id"`
	RoomName    string   `json:"room_name"`
	Password    string   `json:"password,omitempty"`
	MemberToken string   `json:"member_token"`
	Member      Member   `json:"member"`
	WebSocket   string   `json:"websocket_path"`
	VPN         *VPNInfo `json:"vpn,omitempty"`
}

type VPNInfo struct {
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Hub      string `json:"hub"`
	Username string `json:"username"`
	Password string `json:"password"`
	Subnet   string `json:"subnet"`
}

type Snapshot struct {
	RoomID  string        `json:"room_id"`
	Members []Member      `json:"members"`
	History []ChatMessage `json:"history"`
}

type room struct {
	id           string
	name         string
	hub          string
	passwordSalt string
	passwordHash string
	legacyInvite bool
	members      map[string]Member  // keyed by member token
	online       map[string]*client // keyed by member ID
	history      []ChatMessage
	subnet       string
}

type Store struct {
	mu        sync.RWMutex
	rooms     map[string]*room
	statePath string
	vpn       *SoftEther
}

func NewStore() *Store {
	return &Store{rooms: make(map[string]*room)}
}

type savedRoom struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Hub          string            `json:"hub,omitempty"`
	PasswordSalt string            `json:"password_salt,omitempty"`
	PasswordHash string            `json:"password_hash,omitempty"`
	LegacyInvite bool              `json:"legacy_invite,omitempty"`
	InviteCode   string            `json:"invite_code,omitempty"` // migration from the first format
	Subnet       string            `json:"subnet,omitempty"`
	Members      map[string]Member `json:"members"`
}

func OpenStore(path string, vpn *SoftEther) (*Store, error) {
	s := &Store{rooms: make(map[string]*room), statePath: path, vpn: vpn}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read room state: %w", err)
	}
	var saved []savedRoom
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("decode room state: %w", err)
	}
	migrated := false
	for _, r := range saved {
		if !validRoomID(r.ID) || r.Members == nil || (vpn != nil && r.Subnet == "") || (vpn == nil && r.Subnet != "") {
			return nil, errors.New("invalid room state")
		}
		if r.PasswordHash == "" && r.InviteCode != "" {
			// Old invite codes remain usable as room passwords after upgrade.
			r.PasswordSalt, r.PasswordHash, err = derivePassword(r.InviteCode)
			if err != nil {
				return nil, err
			}
			migrated = true
			r.LegacyInvite = true
		}
		if r.PasswordSalt == "" || r.PasswordHash == "" {
			return nil, errors.New("invalid room password state")
		}
		if r.Hub == "" && len(r.ID) == 32 {
			r.Hub = "LR" + r.ID[:24]
		}
		if vpn != nil && r.Hub == "" {
			return nil, errors.New("missing room hub")
		}
		key := normalizeRoomID(r.ID)
		if _, exists := s.rooms[key]; exists {
			return nil, errors.New("duplicate room id in state")
		}
		s.rooms[key] = &room{id: r.ID, name: r.Name, hub: r.Hub, passwordSalt: r.PasswordSalt, passwordHash: r.PasswordHash, legacyInvite: r.LegacyInvite, subnet: r.Subnet, members: r.Members, online: make(map[string]*client)}
	}
	if migrated {
		if err := s.saveLocked(); err != nil {
			return nil, fmt.Errorf("migrate room state: %w", err)
		}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	if s.statePath == "" {
		return nil
	}
	saved := make([]savedRoom, 0, len(s.rooms))
	for _, r := range s.rooms {
		saved = append(saved, savedRoom{ID: r.id, Name: r.name, Hub: r.hub, PasswordSalt: r.passwordSalt, PasswordHash: r.passwordHash, LegacyInvite: r.legacyInvite, Subnet: r.subnet, Members: r.members})
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.statePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".lanroom-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.statePath)
}

func (s *Store) nextSubnetLocked() (string, error) {
	used := make(map[string]bool, len(s.rooms))
	for _, r := range s.rooms {
		used[r.subnet] = true
	}
	for i := 0; i < 2048; i++ {
		prefix := fmt.Sprintf("10.%d.%d", 212+i/256, i%256)
		if !used[prefix+".0/24"] {
			return prefix, nil
		}
	}
	return "", errors.New("no room subnets available")
}

func (s *Store) CreateRoom(ctx context.Context, requestedID, requestedPassword, roomName, memberName string) (JoinResult, error) {
	roomName = strings.TrimSpace(roomName)
	memberName = strings.TrimSpace(memberName)
	if !validName(roomName, maxRoomNameRunes) || !validName(memberName, maxMemberNameRunes) {
		return JoinResult{}, errInvalidName
	}
	roomID := strings.TrimSpace(requestedID)
	var err error
	if roomID == "" {
		roomID, err = randomHex(16)
		if err != nil {
			return JoinResult{}, fmt.Errorf("create room id: %w", err)
		}
	}
	if !validRoomID(roomID) {
		return JoinResult{}, errInvalidRoomID
	}
	password := requestedPassword
	if password == "" {
		password, err = randomToken()
		if err != nil {
			return JoinResult{}, fmt.Errorf("create room password: %w", err)
		}
	}
	if !validPassword(password) {
		return JoinResult{}, errInvalidPassword
	}
	salt, hash, err := derivePassword(password)
	if err != nil {
		return JoinResult{}, fmt.Errorf("hash room password: %w", err)
	}
	memberToken, err := randomToken()
	if err != nil {
		return JoinResult{}, fmt.Errorf("create member token: %w", err)
	}
	memberID, err := randomHex(8)
	if err != nil {
		return JoinResult{}, fmt.Errorf("create member id: %w", err)
	}
	member := Member{ID: memberID, Name: memberName, JoinedAt: time.Now().UTC()}
	var vpnInfo *VPNInfo
	hubSuffix, err := randomHex(12)
	if err != nil {
		return JoinResult{}, fmt.Errorf("create hub id: %w", err)
	}
	hub := "LR" + hubSuffix

	s.mu.Lock()
	defer s.mu.Unlock()
	key := normalizeRoomID(roomID)
	if _, exists := s.rooms[key]; exists {
		return JoinResult{}, errRoomExists
	}
	var subnet string
	if s.vpn != nil {
		prefix, err := s.nextSubnetLocked()
		if err != nil {
			return JoinResult{}, err
		}
		subnet = prefix + ".0/24"
		vpnInfo, err = s.vpn.CreateRoom(ctx, hub, memberID, prefix)
		if err != nil {
			return JoinResult{}, err
		}
	}
	s.rooms[key] = &room{
		id:           roomID,
		name:         roomName,
		hub:          hub,
		passwordSalt: salt,
		passwordHash: hash,
		members:      map[string]Member{memberToken: member},
		online:       make(map[string]*client),
		subnet:       subnet,
	}
	if err := s.saveLocked(); err != nil {
		delete(s.rooms, key)
		if s.vpn != nil {
			_ = s.vpn.DeleteRoom(context.Background(), hub)
		}
		return JoinResult{}, fmt.Errorf("save room: %w", err)
	}

	return JoinResult{
		RoomID:      roomID,
		RoomName:    roomName,
		Password:    password,
		MemberToken: memberToken,
		Member:      member,
		WebSocket:   "/api/rooms/" + roomID + "/ws",
		VPN:         vpnInfo,
	}, nil
}

func (s *Store) JoinRoom(ctx context.Context, roomID, password, memberName string) (JoinResult, error) {
	memberName = strings.TrimSpace(memberName)
	if !validName(memberName, maxMemberNameRunes) {
		return JoinResult{}, errInvalidName
	}
	memberToken, err := randomToken()
	if err != nil {
		return JoinResult{}, fmt.Errorf("create member token: %w", err)
	}
	memberID, err := randomHex(8)
	if err != nil {
		return JoinResult{}, fmt.Errorf("create member id: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[normalizeRoomID(roomID)]
	if ok && r.legacyInvite {
		password = strings.ToUpper(strings.TrimSpace(password))
	}
	if !ok || !passwordMatches(password, r.passwordSalt, r.passwordHash) {
		return JoinResult{}, errRoomNotFound
	}
	member := Member{ID: memberID, Name: memberName, JoinedAt: time.Now().UTC()}
	var vpnInfo *VPNInfo
	if s.vpn != nil {
		vpnInfo, err = s.vpn.AddMember(ctx, r.hub, memberID, r.subnet)
		if err != nil {
			return JoinResult{}, err
		}
	}
	r.members[memberToken] = member
	if err := s.saveLocked(); err != nil {
		delete(r.members, memberToken)
		if s.vpn != nil {
			_ = s.vpn.DeleteMember(context.Background(), r.hub, memberID)
		}
		return JoinResult{}, fmt.Errorf("save member: %w", err)
	}
	return JoinResult{
		RoomID:      r.id,
		RoomName:    r.name,
		MemberToken: memberToken,
		Member:      member,
		WebSocket:   "/api/rooms/" + r.id + "/ws",
		VPN:         vpnInfo,
	}, nil
}

func (s *Store) Connect(roomID, token string, c *client) (Snapshot, error) {
	s.mu.Lock()
	key := normalizeRoomID(roomID)
	r, ok := s.rooms[key]
	if !ok {
		s.mu.Unlock()
		return Snapshot{}, errRoomNotFound
	}
	member, ok := r.members[token]
	if !ok {
		s.mu.Unlock()
		return Snapshot{}, errInvalidToken
	}
	c.member = member
	c.roomID = key
	c.store = s
	previous := r.online[member.ID]
	r.online[member.ID] = c

	snapshot := Snapshot{RoomID: r.id, Members: onlineMembers(r), History: append([]ChatMessage{}, r.history...)}
	joinedEvent := eventBytes("member.joined", member)
	slow := deliverLocked(r, joinedEvent, c)
	s.mu.Unlock()

	if previous != nil && previous != c {
		previous.stop()
	}
	stopClients(slow)
	return snapshot, nil
}

func (s *Store) Disconnect(c *client) {
	s.mu.Lock()
	r, ok := s.rooms[c.roomID]
	if !ok || r.online[c.member.ID] != c {
		s.mu.Unlock()
		return
	}
	delete(r.online, c.member.ID)
	leftEvent := eventBytes("member.left", map[string]string{"member_id": c.member.ID})
	slow := deliverLocked(r, leftEvent, nil)
	s.mu.Unlock()
	stopClients(slow)
}

func (s *Store) PublishChat(c *client, text string) error {
	text = strings.TrimSpace(text)
	if !validName(text, maxChatRunes) {
		return errInvalidChat
	}

	s.mu.Lock()
	r, ok := s.rooms[c.roomID]
	if !ok || r.online[c.member.ID] != c {
		s.mu.Unlock()
		return errNotConnected
	}
	messageID, err := randomHex(8)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("create message id: %w", err)
	}
	message := ChatMessage{
		ID:        messageID,
		MemberID:  c.member.ID,
		Name:      c.member.Name,
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}
	r.history = append(r.history, message)
	if len(r.history) > maxChatHistory {
		r.history = append([]ChatMessage(nil), r.history[len(r.history)-maxChatHistory:]...)
	}
	slow := deliverLocked(r, eventBytes("chat.message", message), nil)
	s.mu.Unlock()
	stopClients(slow)
	return nil
}

func onlineMembers(r *room) []Member {
	members := make([]Member, 0, len(r.online))
	for _, c := range r.online {
		members = append(members, c.member)
	}
	return members
}

func deliverLocked(r *room, data []byte, except *client) []*client {
	var slow []*client
	for _, recipient := range r.online {
		if recipient == except {
			continue
		}
		select {
		case recipient.send <- data:
		default:
			slow = append(slow, recipient)
		}
	}
	return slow
}

func stopClients(clients []*client) {
	for _, c := range clients {
		c.stop()
	}
}

func eventBytes(eventType string, data any) []byte {
	encoded, err := json.Marshal(event{Type: eventType, Data: data})
	if err != nil {
		return []byte(`{"type":"server.error","data":{"message":"encoding error"}}`)
	}
	return encoded
}

func validName(value string, maxRunes int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len([]rune(value)) <= maxRunes
}

func validRoomID(value string) bool {
	if len(value) < 4 || len(value) > 32 {
		return false
	}
	for _, ch := range value {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' {
			continue
		}
		return false
	}
	return true
}

func normalizeRoomID(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func validPassword(value string) bool {
	length := 0
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return false
		}
		length++
	}
	return length >= 6 && length <= 128
}

func derivePassword(value string) (string, string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", "", err
	}
	hash, err := pbkdf2.Key(sha256.New, value, salt, passwordIterations, 32)
	if err != nil {
		return "", "", err
	}
	return base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash), nil
}

func passwordMatches(value, saltText, hashText string) bool {
	salt, err := base64.RawStdEncoding.DecodeString(saltText)
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(hashText)
	if err != nil || len(expected) != 32 {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, value, salt, passwordIterations, 32)
	return err == nil && subtle.ConstantTimeCompare(actual, expected) == 1
}

func randomHex(bytesCount int) (string, error) {
	value := make([]byte, bytesCount)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
