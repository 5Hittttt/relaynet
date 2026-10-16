// Package tunnel implements the relay client: room handshake, keepalive, and
// UDP port forwarding that bridges local game traffic across the relay.
//
// Two modes:
//
//   - Host (ExposePorts): the host's game listens on those ports. Incoming relay
//     packets for a port are delivered to 127.0.0.1:port; responses are tunneled
//     back to the originating joiner (one local socket per joiner so the game can
//     distinguish peers by source port).
//   - Joiner (Forwards): for each {LocalPort L, RemotePort R} a listener is opened
//     on 127.0.0.1:L. Packets from the local game are tunneled to the host's port R;
//     responses are delivered back to the game's source address.
package tunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"relaynet/internal/crypto"
	"relaynet/internal/media"
	"relaynet/internal/proto"
)

// LogFunc receives human-readable status lines (also surfaced in the UI).
type LogFunc func(format string, args ...interface{})

// Forward maps a local listener port to the host's remote game port.
type Forward struct {
	LocalPort  int `json:"localPort"`
	RemotePort int `json:"remotePort"`
}

// Config configures a client session.
type Config struct {
	ServerAddr  string    `json:"serverAddr"`  // host:port of the relay server
	Name        string    `json:"name"`        // display name
	Hostname    string    `json:"hostname"`    // optional custom hostname; empty means use system hostname
	RoomID      string    `json:"roomId"`      // room name
	Password    string    `json:"password"`    // room password
	IsHost      bool      `json:"isHost"`      // create room as host
	Protocol    string    `json:"protocol"`    // "udp" or "tcp"
	ExposePorts []int     `json:"exposePorts"` // host: local game ports to expose
	Forwards    []Forward `json:"forwards"`    // joiner: local -> remote port mappings
}

// Member is a room member as seen by the client.
type Member struct {
	ID       uint32 `json:"id"`
	IsHost   bool   `json:"isHost"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	RTT      int    `json:"rtt"` // round-trip time in milliseconds, 0 if unknown
}

// ChatMsg is a single chat message in the room.
type ChatMsg struct {
	SenderID   uint32 `json:"senderId"`
	SenderName string `json:"senderName"`
	Text       string `json:"text"`
	Mine       bool   `json:"mine"`
}

// Status is the snapshot returned to the UI.
type Status struct {
	Connected           bool           `json:"connected"`
	ClientID            uint32         `json:"clientId"`
	IsHost              bool           `json:"isHost"`
	HostID              uint32         `json:"hostId"`
	GameHostID          uint32         `json:"gameHostId"`
	RoomID              string         `json:"roomId"`
	ServerAddr          string         `json:"serverAddr"`
	ServerRTT           int            `json:"serverRtt"` // to relay server, in ms
	Members             []Member       `json:"members"`
	ExposePorts         []int          `json:"exposePorts"`
	Forwards            []Forward      `json:"forwards"`
	Chat                []ChatMsg      `json:"chat"`
	VideoSharers        []uint32       `json:"videoSharers"` // member IDs currently sharing their screen
	RoomVotingSupported bool           `json:"roomVotingSupported"`
	RoomVote            proto.RoomVote `json:"roomVote"`
	RoomControlAckID    uint64         `json:"roomControlAckId"`
	RoomControlError    string         `json:"roomControlError"`
	RoomControlPending  bool           `json:"roomControlPending"`
}

// Client is a relay client session.
type Client struct {
	cfg        Config
	log        LogFunc
	ourHost    string // resolved hostname sent to peers
	conn       *net.UDPConn
	serverAddr *net.UDPAddr

	clientID            uint32
	hostID              uint32
	gameHostID          uint32
	isHost              bool
	dataKey             []byte
	serverRTT           int // ms, most recent measurement; -1 means not yet measured
	roomVotingSupported bool
	roomStateSequence   uint64
	roomStateReceived   time.Time
	roomVote            proto.RoomVote
	roomControlAckID    uint64
	roomControlError    string
	roomControlNextID   uint64
	roomControlPending  *proto.RoomControl

	sessionMu    sync.Mutex // serializes connection startup and complete cleanup
	mu           sync.Mutex
	members      map[uint32]Member
	expose       map[uint64]*exposeState  // host side: key = port<<32 | joinerID
	fwd          map[int]*forwardState    // joiner side: key = remotePort
	peerPingSent map[uint32]int64         // pending peer pings: memberID -> send time ns
	tcpConns     map[uint32]*tcpConnState // host side: key = connID
	tcpForwards  map[int]*tcpForwardState // joiner side: key = localPort
	nextConnID   uint32                   // next TCP connection ID
	chat         []ChatMsg                // recent chat messages (capped)

	audioCapturer             *media.AudioCapturer
	audioPlayer               *media.AudioPlayer
	systemAudioCapturer       *media.AudioCapturer
	systemAudioPlayer         *media.AudioPlayer
	videoCapturer             *media.VideoCapturer
	screenShare               *screenShareSession
	videoRenderer             *media.VideoRenderer
	audioVolume               float32
	videoQuality              media.VideoQuality
	screenAudioCapturer       *media.AudioCapturer
	screenAudioStop           chan struct{}
	screenAudioDone           chan struct{}
	screenAudioPlayer         *media.ScreenAudioPlayer
	screenTimeline            *media.ScreenTimeline
	screenSource              uint32
	screenSubscriptionID      uint64
	screenPlaybackResolution  byte
	screenPlaybackFPS         byte
	screenSubscriptionSources map[uint32]bool
	screenSelectMu            sync.Mutex
	videoPacerMu              sync.Mutex
	videoPacerTokens          float64
	videoPacerUpdated         time.Time
	peerVolumes               map[uint32]float32   // per-member playback volume (default 1.0)
	videoSharers              map[uint32]time.Time // member IDs actively sharing screen -> last frame time
	videoSharerMu             sync.Mutex           // protects videoSharers

	// fragment reassembly: key = srcID<<32 | frameID
	fragBuf  map[uint64]*fragAssembly
	fragMu   sync.Mutex
	frameSeq uint32 // local frame counter for outgoing fragments

	stop chan struct{}
	wg   sync.WaitGroup
}

const (
	maxVideoEncoderBitrateKbps = 85_000
	videoPaceBytesPerSecond    = 11_000_000 // 88 Mbps payload, leaving room for UDP/IP overhead below 100 Mbps.
	videoPaceBurstBytes        = 64 * 1024  // Keep timer jitter from limiting a 100 Mbps stream to one datagram per wakeup.
)

type fragAssembly struct {
	chunks    [][]byte
	total     int
	received  int
	createdAt time.Time
	frameType byte
}

type exposeState struct {
	remotePort int
	joinerID   uint32
	gameConn   *net.UDPConn // connected to 127.0.0.1:remotePort
}

type forwardState struct {
	remotePort int
	localPort  int
	listener   *net.UDPConn
	gameAddr   *net.UDPAddr
}

type tcpConnState struct {
	connID     uint32
	remotePort int
	joinerID   uint32
	gameConn   *net.TCPConn // connected to 127.0.0.1:remotePort (host side)
}

type tcpForwardState struct {
	remotePort int
	localPort  int
	listener   *net.TCPListener
}

// New creates an unconnected client.
func New(cfg Config, log LogFunc) *Client {
	if log == nil {
		log = func(string, ...interface{}) {}
	}
	ourHost := cfg.Hostname
	if ourHost == "" {
		if h, err := os.Hostname(); err == nil {
			ourHost = h
		}
	}
	return &Client{
		cfg:                       cfg,
		log:                       log,
		ourHost:                   ourHost,
		serverRTT:                 -1,
		members:                   make(map[uint32]Member),
		expose:                    make(map[uint64]*exposeState),
		fwd:                       make(map[int]*forwardState),
		peerPingSent:              make(map[uint32]int64),
		tcpConns:                  make(map[uint32]*tcpConnState),
		tcpForwards:               make(map[int]*tcpForwardState),
		peerVolumes:               make(map[uint32]float32),
		videoSharers:              make(map[uint32]time.Time),
		fragBuf:                   make(map[uint64]*fragAssembly),
		screenSubscriptionSources: make(map[uint32]bool),
	}
}

// Connect performs the handshake and starts the background loops.
func (c *Client) Connect() error {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return errors.New("already connected")
	}
	c.mu.Unlock()

	addr, err := net.ResolveUDPAddr("udp", c.cfg.ServerAddr)
	if err != nil {
		return fmt.Errorf("resolve server: %w", err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	conn.SetReadBuffer(2 * 1024 * 1024)
	conn.SetWriteBuffer(2 * 1024 * 1024)

	c.mu.Lock()
	c.conn = conn
	c.serverAddr = addr
	c.dataKey = crypto.DeriveDataKey(c.cfg.Password, c.cfg.RoomID)
	c.stop = make(chan struct{})
	c.resetRoomVotingLocked()
	c.mu.Unlock()

	if err := c.handshake(); err != nil {
		conn.Close()
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		return err
	}

	// Create a default video renderer so incoming screen-share frames from
	// other members are buffered even when we haven't started our own capture.
	c.mu.Lock()
	if c.videoRenderer == nil {
		c.videoRenderer = media.NewVideoRenderer(nil)
		c.videoRenderer.Start()
	}
	c.mu.Unlock()

	if err := c.startTunnels(); err != nil {
		c.disconnectSessionLocked(conn)
		return err
	}

	c.wg.Add(1)
	go c.readLoop()
	c.wg.Add(1)
	go c.pingLoop()
	c.wg.Add(1)
	go c.screenControlLoop()
	c.wg.Add(1)
	go c.roomControlLoop(conn, c.stop)

	c.mu.Lock()
	cid, isHost := c.clientID, c.isHost
	c.mu.Unlock()
	c.log("connected: room=%s id=%d host=%v", c.cfg.RoomID, cid, isHost)
	return nil
}

func (c *Client) handshake() error {
	hello, err := proto.EncodeHello(proto.Hello{
		Room:      c.cfg.RoomID,
		Name:      c.cfg.Name,
		Hostname:  c.ourHost,
		AuthToken: crypto.AuthToken(c.cfg.Password),
		IsHost:    c.cfg.IsHost,
	})
	if err != nil {
		return err
	}
	if _, err := c.conn.Write(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	buf := make([]byte, 65535)
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})
	var earlyState []byte
	var earlySequence uint64
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			return fmt.Errorf("read join: %w", err)
		}
		p := buf[:n]
		if len(p) == 0 {
			continue
		}
		switch p[0] {
		case proto.TypeReject:
			reason, _ := proto.DecodeReject(p)
			return fmt.Errorf("rejected: %s", reason)
		case proto.TypeRoomState:
			// UDP may reorder the initial snapshot ahead of Joined. Retain
			// its newest valid version until our client ID is established.
			state, err := proto.DecodeRoomState(p)
			if err == nil && (earlyState == nil || state.Sequence > earlySequence) {
				earlyState = append(earlyState[:0], p...)
				earlySequence = state.Sequence
			}
		case proto.TypeJoined:
			cid, isHost, hostID, members, err := proto.DecodeJoined(p)
			if err != nil {
				return fmt.Errorf("decode joined: %w", err)
			}
			c.mu.Lock()
			c.clientID = cid
			c.isHost = isHost
			c.hostID = hostID
			c.members = make(map[uint32]Member, len(members))
			for _, m := range members {
				c.members[m.ID] = Member{ID: m.ID, IsHost: m.IsHost, Name: m.Name, Hostname: m.Hostname, RTT: -1}
				if m.IsHost {
					c.hostID = m.ID
				}
			}
			c.gameHostID = c.hostID
			c.mu.Unlock()
			if earlyState != nil {
				c.handleRoomState(earlyState)
			}
			return nil
		}
	}
}

// startTunnels opens local listeners (joiner) or prepares expose state (host).
func (c *Client) startTunnels() error {
	if c.gameHostID == c.clientID {
		c.log("host mode: exposing ports %v (protocol: %s)", c.cfg.ExposePorts, c.cfg.Protocol)
		return nil
	}
	for _, f := range c.cfg.Forwards {
		if c.cfg.Protocol == "tcp" {
			laddr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.LocalPort}
			lc, err := net.ListenTCP("tcp", laddr)
			if err != nil {
				return fmt.Errorf("listen tcp 127.0.0.1:%d: %w", f.LocalPort, err)
			}
			fs := &tcpForwardState{remotePort: f.RemotePort, localPort: f.LocalPort, listener: lc}
			c.mu.Lock()
			c.tcpForwards[f.LocalPort] = fs
			c.mu.Unlock()
			c.wg.Add(1)
			go c.tcpForwardAcceptLoop(fs)
			c.log("tcp forward: 127.0.0.1:%d -> host:%d", f.LocalPort, f.RemotePort)
		} else {
			laddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.LocalPort}
			lc, err := net.ListenUDP("udp", laddr)
			if err != nil {
				return fmt.Errorf("listen udp 127.0.0.1:%d: %w", f.LocalPort, err)
			}
			lc.SetReadBuffer(1 * 1024 * 1024)
			fs := &forwardState{remotePort: f.RemotePort, localPort: f.LocalPort, listener: lc}
			c.mu.Lock()
			c.fwd[f.RemotePort] = fs
			c.mu.Unlock()
			c.wg.Add(1)
			go c.forwardReadLoop(fs)
			c.log("udp forward: 127.0.0.1:%d -> host:%d", f.LocalPort, f.RemotePort)
		}
	}
	return nil
}

// readLoop handles all incoming packets from the relay server.
func (c *Client) readLoop() {
	defer c.wg.Done()
	c.mu.Lock()
	conn, stop := c.conn, c.stop
	c.mu.Unlock()
	if conn == nil {
		return
	}
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			c.log("read error: %v", err)
			go c.disconnectSession(conn)
			return
		}
		c.mu.Lock()
		current := c.conn == conn
		c.mu.Unlock()
		if !current {
			return
		}
		c.handlePacket(buf[:n])
		if n > 0 && buf[0] == proto.TypeReject {
			return
		}
	}
}

func (c *Client) handlePacket(p []byte) {
	if len(p) == 0 {
		return
	}
	switch p[0] {
	case proto.TypeData:
		c.handleData(p)
	case proto.TypeTCPConnect:
		c.handleTCPConnect(p)
	case proto.TypeTCPData:
		c.handleTCPData(p)
	case proto.TypeTCPClose:
		c.handleTCPClose(p)
	case proto.TypeChat:
		c.handleChat(p)
	case proto.TypeAudioData:
		c.handleAudioData(p)
	case proto.TypeSystemAudioData:
		c.handleSystemAudioData(p)
	case proto.TypeVideoData:
		c.handleVideoData(p)
	case proto.TypeVideoFragment:
		c.handleVideoFragment(p)
	case proto.TypeMediaControl:
		c.handleMediaControl(p)
	case proto.TypeMemberUpdate:
		c.handleMemberUpdate(p)
	case proto.TypeRoomState:
		c.handleRoomState(p)
	case proto.TypeReject:
		reason, _ := proto.DecodeReject(p)
		c.log("server rejected: %s", reason)
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		go c.disconnectSession(conn)
	case proto.TypePong:
		c.handlePong(p)
	}
}

func (c *Client) handleData(p []byte) {
	srcID, _, port, payload, err := proto.DecodeData(p, c.dataKey)
	if err != nil {
		c.log("decode data error: %v", err)
		return
	}
	if port == proto.PeerPingPort {
		c.handlePeerPing(srcID, payload)
		return
	}
	c.mu.Lock()
	isGameHost := c.gameHostID == c.clientID
	gameHostID := c.gameHostID
	c.mu.Unlock()
	if isGameHost {
		c.deliverToGame(port, srcID, payload)
	} else if srcID == gameHostID {
		c.deliverToGameClient(port, payload)
	}
}

// handlePeerPing either echoes back a peer ping or processes a pong payload.
// The payload is an 8-byte big-endian UnixNano timestamp.
// If we sent the ping, the timestamp matches our outstanding entry → compute RTT.
// Otherwise we echo the same bytes back.
func (c *Client) handlePeerPing(srcID uint32, payload []byte) {
	if len(payload) != 8 {
		return
	}
	ts := int64(binary.BigEndian.Uint64(payload))
	c.mu.Lock()
	if sentAt, ok := c.peerPingSent[srcID]; ok && sentAt == ts {
		rtt := time.Duration(time.Now().UnixNano() - ts)
		if rtt < 0 {
			rtt = 0
		}
		ms := int(rtt / time.Millisecond)
		if m, ok := c.members[srcID]; ok {
			m.RTT = ms
			c.members[srcID] = m
		}
		delete(c.peerPingSent, srcID)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	// Echo back verbatim so the sender can measure.
	pkt, err := proto.EncodeData(c.clientID, srcID, c.dataKey, proto.PeerPingPort, payload)
	if err != nil {
		return
	}
	c.writeGamePacket(pkt)
}

// deliverToGame (host side): send payload to the local game server at 127.0.0.1:port,
// using a per-joiner socket so responses can be routed back to the right joiner.
func (c *Client) deliverToGame(port uint16, srcID uint32, payload []byte) {
	key := uint64(port)<<32 | uint64(srcID)
	c.mu.Lock()
	es, ok := c.expose[key]
	if !ok {
		gc, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
		if err != nil {
			c.mu.Unlock()
			c.log("dial game 127.0.0.1:%d: %v", port, err)
			return
		}
		gc.SetReadBuffer(1 * 1024 * 1024)
		es = &exposeState{remotePort: int(port), joinerID: srcID, gameConn: gc}
		c.expose[key] = es
		c.wg.Add(1)
		go c.exposeReadLoop(es)
	}
	gc := es.gameConn
	c.mu.Unlock()

	if _, err := gc.Write(payload); err != nil {
		c.log("write to game: %v", err)
	}
}

// exposeReadLoop (host side): read responses from the local game and tunnel them
// back to the originating joiner.
func (c *Client) exposeReadLoop(es *exposeState) {
	defer c.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, err := es.gameConn.Read(buf)
		if err != nil {
			return
		}
		pkt, err := proto.EncodeData(c.clientID, es.joinerID, c.dataKey, uint16(es.remotePort), buf[:n])
		if err != nil {
			continue
		}
		c.writeGamePacket(pkt)
	}
}

// deliverToGameClient (joiner side): send payload back to the local game via the
// listener for this remote port, addressing the game's last known source address.
func (c *Client) deliverToGameClient(port uint16, payload []byte) {
	c.mu.Lock()
	fs, ok := c.fwd[int(port)]
	var gameAddr *net.UDPAddr
	var listener *net.UDPConn
	if ok {
		gameAddr = fs.gameAddr
		listener = fs.listener
	}
	c.mu.Unlock()
	if listener == nil || gameAddr == nil {
		return
	}
	listener.WriteToUDP(payload, gameAddr)
}

// forwardReadLoop (joiner side): read packets the local game sends to 127.0.0.1:L
// and tunnel them to the host's remote port.
func (c *Client) forwardReadLoop(fs *forwardState) {
	defer c.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, addr, err := fs.listener.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-c.stop:
				return
			default:
			}
			return
		}
		c.mu.Lock()
		fs.gameAddr = addr
		hostID := c.gameHostID
		c.mu.Unlock()
		if hostID == 0 || hostID == c.clientID {
			continue
		}

		pkt, err := proto.EncodeData(c.clientID, hostID, c.dataKey, uint16(fs.remotePort), buf[:n])
		if err != nil {
			continue
		}
		c.writeGamePacket(pkt)
	}
}

// handleTCPConnect (host side): a joiner wants to establish a TCP connection.
// Dial the local game port and set up the relay.
func (c *Client) handleTCPConnect(p []byte) {
	srcID, _, connID, port, err := proto.DecodeTCPConnect(p)
	if err != nil {
		return
	}
	c.mu.Lock()
	if c.gameHostID != c.clientID {
		c.mu.Unlock()
		return
	}
	if _, ok := c.tcpConns[connID]; ok {
		c.mu.Unlock()
		return
	}
	gc, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
	if err != nil {
		c.mu.Unlock()
		c.log("tcp connect dial game 127.0.0.1:%d: %v", port, err)
		return
	}
	ts := &tcpConnState{connID: connID, remotePort: int(port), joinerID: srcID, gameConn: gc}
	c.tcpConns[connID] = ts
	c.mu.Unlock()
	c.wg.Add(1)
	go c.tcpRelayLoop(ts)
	c.log("tcp connected: conn=%d port=%d joiner=%d", connID, port, srcID)
}

// handleTCPData: deliver encrypted TCP data to the appropriate connection.
func (c *Client) handleTCPData(p []byte) {
	srcID, dstID, connID, payload, err := proto.DecodeTCPData(p, c.dataKey)
	if err != nil {
		return
	}
	if dstID != c.clientID {
		return
	}
	c.mu.Lock()
	ts, ok := c.tcpConns[connID]
	c.mu.Unlock()
	if !ok || ts.joinerID != srcID {
		return
	}
	ts.gameConn.Write(payload)
}

// handleTCPClose: close the local TCP connection and clean up state.
func (c *Client) handleTCPClose(p []byte) {
	srcID, _, connID, err := proto.DecodeTCPClose(p)
	if err != nil {
		return
	}
	c.mu.Lock()
	ts, ok := c.tcpConns[connID]
	if ok && ts.joinerID == srcID {
		ts.gameConn.Close()
		delete(c.tcpConns, connID)
	}
	c.mu.Unlock()
	c.log("tcp closed: conn=%d by=%d", connID, srcID)
}

// tcpForwardAcceptLoop (joiner side): accept local TCP connections and request
// tunneled connections to the host.
func (c *Client) tcpForwardAcceptLoop(fs *tcpForwardState) {
	defer c.wg.Done()
	for {
		gameConn, err := fs.listener.AcceptTCP()
		if err != nil {
			select {
			case <-c.stop:
				return
			default:
			}
			return
		}
		c.mu.Lock()
		hostID := c.gameHostID
		if c.conn == nil || hostID == 0 || hostID == c.clientID {
			c.mu.Unlock()
			gameConn.Close()
			continue
		}
		c.nextConnID++
		connID := c.nextConnID
		ts := &tcpConnState{connID: connID, remotePort: fs.remotePort, joinerID: hostID, gameConn: gameConn}
		c.tcpConns[connID] = ts
		c.wg.Add(1)
		c.mu.Unlock()

		pkt := proto.EncodeTCPConnect(c.clientID, hostID, connID, uint16(fs.remotePort))
		c.writeGamePacket(pkt)

		go c.tcpJoinerRelayLoop(ts, connID)
		c.log("tcp forward accept: conn=%d local=%d remote=%d", connID, fs.localPort, fs.remotePort)
	}
}

// tcpRelayLoop (host side): relay data between the tunnel and the local game server.
func (c *Client) tcpRelayLoop(ts *tcpConnState) {
	defer c.wg.Done()
	buf := make([]byte, 8192)
	for {
		n, err := ts.gameConn.Read(buf)
		if err != nil {
			pkt := proto.EncodeTCPClose(c.clientID, ts.joinerID, ts.connID)
			c.writeGamePacket(pkt)
			c.mu.Lock()
			delete(c.tcpConns, ts.connID)
			c.mu.Unlock()
			return
		}
		pkt, err := proto.EncodeTCPData(c.clientID, ts.joinerID, ts.connID, c.dataKey, buf[:n])
		if err != nil {
			continue
		}
		c.writeGamePacket(pkt)
	}
}

// tcpJoinerRelayLoop (joiner side): relay data between the tunnel and the local game client.
func (c *Client) tcpJoinerRelayLoop(ts *tcpConnState, connID uint32) {
	defer c.wg.Done()
	defer ts.gameConn.Close()
	defer func() {
		c.mu.Lock()
		delete(c.tcpConns, connID)
		c.mu.Unlock()
	}()
	buf := make([]byte, 8192)
	for {
		n, err := ts.gameConn.Read(buf)
		if err != nil {
			pkt := proto.EncodeTCPClose(c.clientID, ts.joinerID, connID)
			c.writeGamePacket(pkt)
			return
		}
		pkt, err := proto.EncodeTCPData(c.clientID, ts.joinerID, connID, c.dataKey, buf[:n])
		if err != nil {
			continue
		}
		c.writeGamePacket(pkt)
	}
}

// handleChat receives a broadcasted chat message and stores it.
func (c *Client) handleChat(p []byte) {
	srcID, senderName, msg, err := proto.DecodeChatBroadcast(p)
	if err != nil {
		return
	}
	c.mu.Lock()
	c.chat = append(c.chat, ChatMsg{
		SenderID:   srcID,
		SenderName: senderName,
		Text:       msg,
		Mine:       srcID == c.clientID,
	})
	if len(c.chat) > 100 {
		c.chat = c.chat[len(c.chat)-100:]
	}
	c.mu.Unlock()
}

// SendChat sends a text message to everyone in the room.
func (c *Client) SendChat(msg string) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	pkt := proto.EncodeChat(msg)
	if _, err := conn.Write(pkt); err != nil {
		return err
	}
	return nil
}

func (c *Client) handleAudioData(p []byte) {
	srcID, dstID, payload, err := proto.DecodeAudioData(p, c.dataKey)
	if err != nil {
		return
	}
	if dstID != c.clientID && dstID != proto.BroadcastID {
		return
	}
	c.mu.Lock()
	ap := c.audioPlayer
	c.mu.Unlock()
	if ap != nil {
		samples := media.DecodeADPCM(payload)
		vol := c.GetPeerVolume(srcID)
		if vol != 1.0 {
			for i := range samples {
				samples[i] *= vol
			}
		}
		ap.Play(samples)
	}
}

func (c *Client) handleSystemAudioData(p []byte) {
	srcID, dstID, payload, err := proto.DecodeSystemAudioData(p, c.dataKey)
	if err != nil {
		return
	}
	if dstID != c.clientID && dstID != proto.BroadcastID {
		return
	}
	meta, data, marked, decodeErr := proto.DecodeScreenPayload(payload)
	if decodeErr != nil {
		return
	}
	if marked {
		if meta.Kind == proto.ScreenAudio {
			c.receiveScreenAudio(srcID, meta, data)
		}
		return
	}
	samples := media.DecodeADPCM(payload)
	vol := c.GetPeerVolume(srcID)
	if vol != 1.0 {
		for i := range samples {
			samples[i] *= vol
		}
	}

	// System audio has its own playback stream so room members can hear it
	// without enabling their microphone.
	c.mu.Lock()
	ap := c.systemAudioPlayer
	if ap == nil && c.conn != nil {
		ap, err = media.NewAudioPlayer(-1)
		if err == nil {
			err = ap.Start()
		}
		if err != nil {
			if ap != nil {
				ap.Close()
			}
			ap = nil
			c.log("system audio playback unavailable: %v", err)
		} else {
			c.systemAudioPlayer = ap
		}
	}
	c.mu.Unlock()
	if ap != nil {
		ap.Play(samples)
	}
}

func (c *Client) handleVideoData(p []byte) {
	srcID, dstID, frameType, payload, err := proto.DecodeVideoData(p, c.dataKey)
	if err != nil {
		return
	}
	if dstID != c.clientID && dstID != proto.BroadcastID {
		return
	}
	// Track that srcID is actively sharing their screen.
	c.videoSharerMu.Lock()
	c.videoSharers[srcID] = time.Now()
	c.videoSharerMu.Unlock()
	c.mu.Lock()
	vr := c.videoRenderer
	c.mu.Unlock()
	if vr != nil {
		c.renderScreenVideo(srcID, frameType, payload)
	}
}

// handleVideoFragment reassembles fragmented video frames, then processes
// them as regular video data.
func (c *Client) handleVideoFragment(p []byte) {
	srcID, dstID, frameID, frameType, fragIndex, fragTotal, err := proto.VideoFragmentHeader(p)
	if err != nil {
		return
	}
	if dstID != c.clientID && dstID != proto.BroadcastID {
		return
	}

	chunk := proto.VideoFragmentChunk(p)
	key := uint64(srcID)<<32 | uint64(frameID)

	c.fragMu.Lock()
	fa, ok := c.fragBuf[key]
	if !ok {
		fa = &fragAssembly{
			chunks:    make([][]byte, fragTotal),
			total:     int(fragTotal),
			createdAt: time.Now(),
			frameType: frameType,
		}
		c.fragBuf[key] = fa
		// garbage-collect stale entries older than 5 seconds
		now := time.Now()
		for k, v := range c.fragBuf {
			if now.Sub(v.createdAt) > 5*time.Second {
				delete(c.fragBuf, k)
			}
		}
	}
	if int(fragIndex) < len(fa.chunks) && fa.chunks[fragIndex] == nil {
		fa.chunks[fragIndex] = append([]byte(nil), chunk...)
		fa.received++
	}

	complete := fa.received == fa.total
	if complete {
		delete(c.fragBuf, key)
	}
	c.fragMu.Unlock()

	if !complete {
		return
	}

	// Reassemble the encrypted blob
	blobSize := 0
	for _, c := range fa.chunks {
		blobSize += len(c)
	}
	blob := make([]byte, 0, blobSize)
	for _, c := range fa.chunks {
		blob = append(blob, c...)
	}

	// Decrypt
	payload, err := crypto.Open(c.dataKey, blob)
	if err != nil {
		return
	}

	// Track sharer
	c.videoSharerMu.Lock()
	c.videoSharers[srcID] = time.Now()
	c.videoSharerMu.Unlock()

	c.mu.Lock()
	vr := c.videoRenderer
	c.mu.Unlock()
	if vr != nil {
		c.renderScreenVideo(srcID, fa.frameType, payload)
	}
}

func (c *Client) handleMediaControl(p []byte) {
	srcID, dstID, ctrlType, data, err := proto.DecodeMediaControl(p)
	if err != nil {
		return
	}
	if dstID != c.clientID && dstID != proto.BroadcastID {
		return
	}
	switch ctrlType {
	case proto.MediaCtrlStartAudio:
	case proto.MediaCtrlStopAudio:
	case proto.MediaCtrlStartVideo:
		c.videoSharerMu.Lock()
		c.videoSharers[srcID] = time.Now()
		c.videoSharerMu.Unlock()
		if len(data) == 1 && data[0] == 2 {
			c.mu.Lock()
			upgrade := !c.screenSubscriptionSources[srcID]
			c.screenSubscriptionSources[srcID] = true
			selected := c.screenSource == srcID
			if upgrade && selected {
				c.screenSubscriptionID++
			}
			c.mu.Unlock()
			if upgrade && selected {
				c.sendScreenSubscription(srcID, true)
			}
		}
	case proto.MediaCtrlStopVideo:
		c.videoSharerMu.Lock()
		delete(c.videoSharers, srcID)
		c.videoSharerMu.Unlock()
	case proto.MediaCtrlSetAudioVolume:
	case proto.MediaCtrlSetVideoQuality:
	case proto.MediaCtrlScreenSubscribe:
		request, err := proto.DecodeScreenSubscription(data)
		if err != nil || dstID == proto.BroadcastID {
			return
		}
		c.mu.Lock()
		share := c.screenShare
		_, member := c.members[srcID]
		c.mu.Unlock()
		if share != nil && member {
			share.request(srcID, request)
		}
	}
}

func (c *Client) handleMemberUpdate(p []byte) {
	event, m, err := proto.DecodeMemberUpdate(p)
	if err != nil {
		return
	}
	c.mu.Lock()
	if c.roomVotingSupported {
		c.mu.Unlock()
		return
	}
	switch event {
	case proto.EventJoin:
		c.members[m.ID] = Member{ID: m.ID, IsHost: m.IsHost, Name: m.Name, Hostname: m.Hostname}
		if m.IsHost {
			c.hostID = m.ID
			c.setGameHostLocked(m.ID)
		}
	case proto.EventLeave:
		c.removeMemberLocked(m.ID)
		if c.hostID == m.ID {
			c.hostID = 0
		}
		if c.gameHostID == m.ID {
			c.gameHostID = 0
		}
	}
	c.isHost = c.clientID == c.hostID
	for id, member := range c.members {
		member.IsHost = id == c.hostID
		c.members[id] = member
	}
	members := c.snapshotMembersLocked()
	c.mu.Unlock()

	verb := "joined"
	if event == proto.EventLeave {
		verb = "left"
	}
	c.log("%s %s the room (%d members)", m.Name, verb, len(members))
}

func (c *Client) pingLoop() {
	defer c.wg.Done()
	c.mu.Lock()
	stop := c.stop
	c.mu.Unlock()
	// Send first server ping quickly so the UI gets an initial RTT.
	c.sendServerPing()
	initial := time.NewTimer(time.Second)
	defer initial.Stop()
	select {
	case <-stop:
		return
	case <-initial.C:
	}
	c.sendPeerPings()

	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.sendServerPing()
			c.sendPeerPings()
		}
	}
}

func (c *Client) sendServerPing() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Write(proto.EncodePing(time.Now().UnixNano()))
	}
}

func (c *Client) sendPeerPings() {
	ts := time.Now().UnixNano()
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, uint64(ts))

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return
	}
	for id, m := range c.members {
		if id == c.clientID {
			continue
		}
		c.peerPingSent[id] = ts
		pkt, err := proto.EncodeData(c.clientID, id, c.dataKey, proto.PeerPingPort, payload)
		if err != nil {
			continue
		}
		// Use a distinct timestamp per member to avoid ambiguity if two peers
		// share the same nanosecond tick; but a single batch-tick is fine for
		// latency measurement and keeps things simple. We store per-member.
		_ = m
		c.conn.Write(pkt)
	}
}

// handlePong updates the server RTT estimate.
func (c *Client) handlePong(p []byte) {
	ts, err := proto.DecodePong(p)
	if err != nil {
		c.log("decode pong error: %v", err)
		return
	}
	if ts == 0 {
		return
	}
	rtt := time.Now().UnixNano() - ts
	if rtt < 0 {
		rtt = 0
	}
	ms := int(rtt / int64(time.Millisecond))
	c.mu.Lock()
	c.serverRTT = ms
	c.mu.Unlock()
	c.log("server RTT: %d ms", ms)
}

// Status returns a snapshot of the current session state.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{
		Connected:           c.conn != nil,
		ClientID:            c.clientID,
		IsHost:              c.isHost,
		HostID:              c.hostID,
		GameHostID:          c.gameHostID,
		RoomID:              c.cfg.RoomID,
		ServerAddr:          c.cfg.ServerAddr,
		ServerRTT:           c.serverRTT,
		Members:             c.snapshotMembersLocked(),
		ExposePorts:         append([]int(nil), c.cfg.ExposePorts...),
		Forwards:            append([]Forward(nil), c.cfg.Forwards...),
		Chat:                append([]ChatMsg(nil), c.chat...),
		VideoSharers:        c.snapshotVideoSharersLocked(),
		RoomVotingSupported: c.roomVotingSupported,
		RoomVote:            c.roomVoteSnapshotLocked(),
		RoomControlAckID:    c.roomControlAckID,
		RoomControlError:    c.roomControlError,
		RoomControlPending:  c.roomControlPending != nil,
	}
	return st
}

// snapshotVideoSharersLocked returns member IDs that sent a video frame within
// the last 3 seconds. Caller must NOT hold c.mu (videoSharers has its own lock).
func (c *Client) snapshotVideoSharersLocked() []uint32 {
	c.videoSharerMu.Lock()
	defer c.videoSharerMu.Unlock()
	now := time.Now()
	out := make([]uint32, 0, len(c.videoSharers))
	for id, last := range c.videoSharers {
		if now.Sub(last) < 3*time.Second {
			out = append(out, id)
		} else {
			delete(c.videoSharers, id) // expired, stop showing
		}
	}
	c.log("videoSharers snapshot: %d active %v", len(out), out)
	return out
}

func (c *Client) snapshotMembersLocked() []Member {
	out := make([]Member, 0, len(c.members))
	for _, m := range c.members {
		out = append(out, m)
	}
	return out
}

// Disconnect closes the session and all local sockets.
func (c *Client) Disconnect() {
	c.disconnectLocked()
}

func (c *Client) disconnectLocked() {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	c.disconnectSessionLocked(conn)
}

// An old reader may report an error after a new connection is established.
func (c *Client) disconnectSession(conn *net.UDPConn) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.disconnectSessionLocked(conn)
}

func (c *Client) disconnectSessionLocked(expected *net.UDPConn) {
	c.screenSelectMu.Lock()
	defer c.screenSelectMu.Unlock()
	c.mu.Lock()
	if c.conn == nil || c.conn != expected {
		c.mu.Unlock()
		return
	}
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	c.conn.Write(proto.EncodeLeave())
	conn := c.conn
	c.conn = nil
	audioCapturer := c.audioCapturer
	audioPlayer := c.audioPlayer
	systemAudioCapturer := c.systemAudioCapturer
	systemAudioPlayer := c.systemAudioPlayer
	screenAudioCapturer, screenAudioStop, screenAudioDone := c.screenAudioCapturer, c.screenAudioStop, c.screenAudioDone
	screenAudioPlayer := c.screenAudioPlayer
	c.screenAudioCapturer, c.screenAudioStop, c.screenAudioDone = nil, nil, nil
	c.screenAudioPlayer, c.screenTimeline, c.screenSource = nil, nil, 0
	videoCapturer := c.videoCapturer
	screenShare := c.screenShare
	c.screenShare = nil
	videoRenderer := c.videoRenderer
	c.audioCapturer = nil
	c.audioPlayer = nil
	c.systemAudioCapturer = nil
	c.systemAudioPlayer = nil
	c.videoCapturer = nil
	c.videoRenderer = nil

	for _, fs := range c.fwd {
		fs.listener.Close()
	}
	c.fwd = make(map[int]*forwardState)
	for _, es := range c.expose {
		es.gameConn.Close()
	}
	c.expose = make(map[uint64]*exposeState)
	for _, ts := range c.tcpConns {
		ts.gameConn.Close()
	}
	c.tcpConns = make(map[uint32]*tcpConnState)
	for _, fs := range c.tcpForwards {
		fs.listener.Close()
	}
	c.tcpForwards = make(map[int]*tcpForwardState)

	c.members = make(map[uint32]Member)
	c.screenSubscriptionSources = make(map[uint32]bool)
	c.peerPingSent = make(map[uint32]int64)
	c.peerVolumes = make(map[uint32]float32)
	c.videoSharerMu.Lock()
	c.videoSharers = make(map[uint32]time.Time)
	c.videoSharerMu.Unlock()
	c.fragMu.Lock()
	c.fragBuf = make(map[uint64]*fragAssembly)
	c.fragMu.Unlock()
	c.chat = nil
	c.serverRTT = -1
	c.hostID, c.gameHostID, c.isHost = 0, 0, false
	c.resetRoomVotingLocked()
	c.mu.Unlock()

	conn.Close()
	if screenShare != nil {
		screenShare.close()
	}
	if screenAudioStop != nil {
		close(screenAudioStop)
	}
	if screenAudioCapturer != nil {
		screenAudioCapturer.Close()
		<-screenAudioDone
	}
	if screenAudioPlayer != nil {
		screenAudioPlayer.Close()
	}
	c.wg.Wait()
	if audioCapturer != nil {
		audioCapturer.Close()
	}
	if audioPlayer != nil {
		audioPlayer.Close()
	}
	if systemAudioCapturer != nil {
		systemAudioCapturer.Close()
	}
	if systemAudioPlayer != nil {
		systemAudioPlayer.Close()
	}
	if videoCapturer != nil {
		videoCapturer.Close()
	}
	if videoRenderer != nil {
		videoRenderer.Close()
	}
	c.log("disconnected")
}

func (c *Client) StartAudio(deviceID int, playbackDeviceID int, sampleRate int, volume float32) error {
	c.mu.Lock()
	if c.audioCapturer != nil {
		c.mu.Unlock()
		return errors.New("audio already started")
	}
	c.audioVolume = volume
	c.mu.Unlock()

	capturer, err := media.NewAudioCapturer(deviceID, sampleRate, func(samples []float32) {
		c.mu.Lock()
		volume := c.audioVolume
		c.mu.Unlock()
		for i := range samples {
			samples[i] *= volume
		}
		encoded := media.EncodeADPCM(samples)
		pkt, err := proto.EncodeAudioData(c.clientID, proto.BroadcastID, c.dataKey, encoded)
		if err == nil {
			c.mu.Lock()
			if c.conn != nil {
				c.conn.Write(pkt)
			}
			c.mu.Unlock()
		}
	})
	if err != nil {
		return err
	}

	player, err := media.NewAudioPlayer(playbackDeviceID)
	if err != nil {
		capturer.Close()
		return err
	}

	c.mu.Lock()
	c.audioCapturer = capturer
	c.audioPlayer = player
	c.mu.Unlock()

	if err := player.Start(); err != nil {
		c.audioCapturer.Close()
		c.audioPlayer.Close()
		c.mu.Lock()
		c.audioCapturer = nil
		c.audioPlayer = nil
		c.mu.Unlock()
		return err
	}

	if err := capturer.Start(); err != nil {
		c.audioCapturer.Close()
		c.audioPlayer.Close()
		c.mu.Lock()
		c.audioCapturer = nil
		c.audioPlayer = nil
		c.mu.Unlock()
		return err
	}

	c.log("audio started: device=%d sampleRate=%d volume=%.2f", deviceID, sampleRate, volume)
	return nil
}

func (c *Client) StopAudio() {
	c.mu.Lock()
	capturer := c.audioCapturer
	player := c.audioPlayer
	c.audioCapturer = nil
	c.audioPlayer = nil
	c.mu.Unlock()
	if capturer != nil {
		capturer.Close()
	}
	if player != nil {
		player.Close()
	}
	c.log("audio stopped")
}

// StartSystemAudio shares the current Windows desktop playback mix with the room.
func (c *Client) StartSystemAudio(sampleRate int) error {
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return errors.New("not connected")
	}
	if c.systemAudioCapturer != nil {
		c.mu.Unlock()
		return errors.New("system audio already started")
	}
	sourceID := c.clientID
	dataKey := c.dataKey
	c.mu.Unlock()

	capturer, err := media.NewSystemAudioCapturer(sampleRate, func(samples []float32) {
		encoded := media.EncodeADPCM(samples)
		pkt, err := proto.EncodeSystemAudioData(sourceID, proto.BroadcastID, dataKey, encoded)
		if err != nil {
			return
		}
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Write(pkt)
		}
		c.mu.Unlock()
	})
	if err != nil {
		return err
	}
	if err := capturer.Start(); err != nil {
		capturer.Close()
		return err
	}

	c.mu.Lock()
	connected := c.conn != nil
	alreadyRunning := c.systemAudioCapturer != nil
	if !connected || alreadyRunning {
		c.mu.Unlock()
		capturer.Close()
		if !connected {
			return errors.New("not connected")
		}
		return errors.New("system audio already started")
	}
	c.systemAudioCapturer = capturer
	c.mu.Unlock()
	c.log("system audio sharing started: sampleRate=%d", sampleRate)
	return nil
}

func (c *Client) StopSystemAudio() {
	c.mu.Lock()
	capturer := c.systemAudioCapturer
	c.systemAudioCapturer = nil
	c.mu.Unlock()
	if capturer != nil {
		capturer.Close()
	}
	c.log("system audio sharing stopped")
}

func (c *Client) SetAudioVolume(volume float32) {
	c.mu.Lock()
	c.audioVolume = volume
	c.mu.Unlock()
}

// SetPeerVolume sets the local playback volume for a specific member's audio.
// Volume is in [0.0, 1.0]; members without an entry default to 1.0 (full).
func (c *Client) SetPeerVolume(memberID uint32, volume float32) {
	c.mu.Lock()
	c.peerVolumes[memberID] = volume
	c.mu.Unlock()
}

// GetPeerVolume returns the local playback volume for a member (default 1.0).
func (c *Client) GetPeerVolume(memberID uint32) float32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if vol, ok := c.peerVolumes[memberID]; ok {
		return vol
	}
	return 1.0
}

// KickMember requests the server to remove a member from the room.
// Only the room host may kick; the server validates the sender's role.
func (c *Client) KickMember(memberID uint32) error {
	c.mu.Lock()
	conn := c.conn
	isHost := c.isHost
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	if !isHost {
		return errors.New("only the host can kick members")
	}
	pkt := proto.EncodeKick(memberID)
	if _, err := conn.Write(pkt); err != nil {
		return err
	}
	return nil
}

func (c *Client) StartVideo(displayIndex int, quality media.VideoQuality, method media.CaptureMethod, audioQuality media.ScreenAudioQuality, previewCB func([]byte, byte)) error {
	if quality.Bitrate > maxVideoEncoderBitrateKbps {
		quality.Bitrate = maxVideoEncoderBitrateKbps
	}
	c.mu.Lock()
	if c.videoCapturer != nil {
		c.mu.Unlock()
		return errors.New("video already started")
	}
	if c.conn == nil {
		c.mu.Unlock()
		return errors.New("not connected")
	}
	c.videoQuality = quality
	c.mu.Unlock()
	if audioQuality != media.ScreenAudioOff {
		c.StopSystemAudio()
	}
	streamID := uint64(time.Now().UnixNano())
	startPTS := media.CaptureTimestamp()
	var audioCapturer *media.AudioCapturer
	var audioStop, audioDone chan struct{}
	if audioQuality != media.ScreenAudioOff {
		var err error
		audioCapturer, audioStop, audioDone, err = c.startScreenAudio(streamID, startPTS, audioQuality)
		if err != nil {
			return err
		}
	}

	resolved := media.ResolveMethod(method)
	share := &screenShareSession{client: c, quality: quality, streamID: streamID, startPTS: startPTS,
		hasAudio: audioQuality != media.ScreenAudioOff, viewers: make(map[uint32]screenViewerRequest),
		stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1)}
	frameCount := 0
	capturer := media.NewVideoCapturer(displayIndex, quality, method, func(frame media.VideoFrame) {
		frameCount++
		if frameCount == 1 {
			c.log("video: first frame produced (len=%d, type=%d)", len(frame.Data), frame.Type)
		}

		share.send(frame, uint32(frameCount), quality, true)

		if previewCB != nil {
			previewCB(frame.Data, frame.Type)
		}
		// Keep ourselves in the sharers list as long as we're producing frames.
		c.videoSharerMu.Lock()
		c.videoSharers[c.clientID] = time.Now()
		c.videoSharerMu.Unlock()
	})

	c.mu.Lock()
	if c.conn == nil || c.videoCapturer != nil {
		c.mu.Unlock()
		if audioStop != nil {
			close(audioStop)
			audioCapturer.Close()
			<-audioDone
		}
		return errors.New("screen sharing state changed during startup")
	}
	c.screenAudioCapturer, c.screenAudioStop, c.screenAudioDone = audioCapturer, audioStop, audioDone
	c.videoCapturer = capturer
	share.base = capturer
	c.screenShare = share
	// Keep the existing video renderer (created in Connect) so incoming
	// frames from other members continue to work while we share our own screen.
	c.mu.Unlock()

	capturer.Start()
	go share.run()

	// Mark ourselves as actively sharing so other members (and our own UI)
	// can see us in the sharers list.
	c.videoSharerMu.Lock()
	c.videoSharers[c.clientID] = time.Now()
	c.videoSharerMu.Unlock()

	methodName := media.CaptureMethodNames[resolved]
	c.log("video started: display=%d quality=%dx%d@%dfps method=%s", displayIndex, quality.Width, quality.Height, quality.FPS, methodName)
	return nil
}

// writeVideoPacket paces screen-sharing datagrams so bursts from the encoder
// cannot exceed the configured per-client video bandwidth budget.
func (c *Client) writeVideoPacket(conn *net.UDPConn, packet []byte) {
	if conn == nil || len(packet) == 0 {
		return
	}
	c.mu.Lock()
	stop := c.stop
	connected := c.conn != nil
	c.mu.Unlock()
	if !connected {
		return
	}

	for {
		now := time.Now()
		c.videoPacerMu.Lock()
		if c.videoPacerUpdated.IsZero() {
			c.videoPacerTokens = videoPaceBurstBytes
		} else {
			elapsed := now.Sub(c.videoPacerUpdated).Seconds()
			c.videoPacerTokens += elapsed * videoPaceBytesPerSecond
			if c.videoPacerTokens > videoPaceBurstBytes {
				c.videoPacerTokens = videoPaceBurstBytes
			}
		}
		c.videoPacerUpdated = now
		if c.videoPacerTokens >= float64(len(packet)) {
			c.videoPacerTokens -= float64(len(packet))
			c.videoPacerMu.Unlock()
			conn.Write(packet)
			return
		}
		wait := time.Duration((float64(len(packet)) - c.videoPacerTokens) / videoPaceBytesPerSecond * float64(time.Second))
		c.videoPacerMu.Unlock()
		if wait < time.Microsecond {
			wait = time.Microsecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-stop:
			timer.Stop()
			return
		}
	}
}

func (c *Client) StopVideo() {
	c.mu.Lock()
	capturer, audio, stop, done := c.videoCapturer, c.screenAudioCapturer, c.screenAudioStop, c.screenAudioDone
	share := c.screenShare
	c.screenShare = nil
	c.videoCapturer, c.screenAudioCapturer, c.screenAudioStop, c.screenAudioDone = nil, nil, nil, nil
	cid := c.clientID
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		conn.Write(proto.EncodeMediaControl(cid, proto.BroadcastID, proto.MediaCtrlStopVideo, nil))
	}
	if share != nil {
		share.close()
	}
	if stop != nil {
		close(stop)
	}
	if audio != nil {
		audio.Close()
		<-done
	}
	if capturer != nil {
		capturer.Close()
	}
	c.videoSharerMu.Lock()
	delete(c.videoSharers, cid)
	c.videoSharerMu.Unlock()
	c.log("video stopped")
}

func (c *Client) SetVideoQuality(quality media.VideoQuality) {
	if quality.Bitrate > maxVideoEncoderBitrateKbps {
		quality.Bitrate = maxVideoEncoderBitrateKbps
	}
	c.mu.Lock()
	c.videoQuality = quality
	if c.videoCapturer != nil {
		c.videoCapturer.SetQuality(quality)
	}
	c.mu.Unlock()
}

// GetLatestFrame returns the most recently received video frame as a
// media.VideoFrame (Data + codec type), or a zero frame if none yet.
func (c *Client) GetLatestFrame() media.VideoFrame {
	c.mu.Lock()
	vr := c.videoRenderer
	c.mu.Unlock()
	if vr == nil {
		return media.VideoFrame{}
	}
	return vr.LatestFrame()
}

// GetLatestFrameSeq returns the current frame sequence number and the frame
// itself (Data + codec type). The UI can compare the sequence number to
// detect new frames.
func (c *Client) GetLatestFrameSeq() (uint64, media.VideoFrame) {
	c.mu.Lock()
	vr := c.videoRenderer
	c.mu.Unlock()
	if vr == nil {
		return 0, media.VideoFrame{}
	}
	return vr.LatestFrameSeq()
}

// GetVideoFramesSince returns received frames in order so the UI can feed
// every H.264 reference frame to its decoder before drawing the newest image.
func (c *Client) GetVideoFramesSince(lastSeq uint64) (uint64, []media.VideoFrame, bool) {
	c.mu.Lock()
	vr := c.videoRenderer
	c.mu.Unlock()
	if vr == nil {
		return lastSeq, nil, false
	}
	return vr.FramesSince(lastSeq)
}
