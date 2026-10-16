package app

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const maxRequestBytes = 16 * 1024

type event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type Server struct {
	store      *Store
	vpnEnabled bool
	logger     *slog.Logger
	upgrader   websocket.Upgrader
}

func New(logger *slog.Logger) *Server {
	server, _ := NewWithConfig(logger, "", nil)
	return server
}

func NewWithConfig(logger *slog.Logger, statePath string, vpn *SoftEther) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var store *Store
	var err error
	if statePath == "" {
		store = NewStore()
	} else {
		store, err = OpenStore(statePath, vpn)
	}
	if err != nil {
		return nil, err
	}
	return &Server{
		store:      store,
		vpnEnabled: vpn != nil,
		logger:     logger,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// The desktop client authenticates each WebSocket with a random bearer
			// token. It does not rely on browser cookies or ambient credentials.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/rooms", s.handleCreateRoom)
	mux.HandleFunc("POST /api/rooms/{roomID}/join", s.handleJoinRoom)
	mux.HandleFunc("GET /api/rooms/{roomID}/ws", s.handleWebSocket)
	return requestLogger(s.logger, mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"service":     "lanroom-server",
		"vpn_enabled": s.vpnEnabled,
	})
}

func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name        string `json:"name"`
		RoomID      string `json:"room_id"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	result, err := s.store.CreateRoom(r.Context(), input.RoomID, input.Password, input.Name, input.DisplayName)
	if errors.Is(err, errInvalidName) {
		writeError(w, http.StatusBadRequest, "invalid_name", "room name or display name is empty or too long")
		return
	}
	if errors.Is(err, errInvalidRoomID) {
		writeError(w, http.StatusBadRequest, "invalid_room_id", "room ID must be 4-32 letters, digits, hyphens or underscores")
		return
	}
	if errors.Is(err, errInvalidPassword) {
		writeError(w, http.StatusBadRequest, "invalid_password", "room password must contain 6-128 characters without control characters")
		return
	}
	if errors.Is(err, errRoomExists) {
		writeError(w, http.StatusConflict, "room_exists", "room ID already exists")
		return
	}
	if err != nil {
		s.logger.Error("create room failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create room")
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleJoinRoom(w http.ResponseWriter, r *http.Request) {
	var input struct {
		InviteCode  string `json:"invite_code"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	password := input.Password
	if password == "" {
		password = input.InviteCode
	}
	result, err := s.store.JoinRoom(r.Context(), r.PathValue("roomID"), password, input.DisplayName)
	if errors.Is(err, errInvalidName) {
		writeError(w, http.StatusBadRequest, "invalid_name", "display name is empty or too long")
		return
	}
	if errors.Is(err, errRoomNotFound) {
		writeError(w, http.StatusNotFound, "room_not_found", "room was not found or password is invalid")
		return
	}
	if err != nil {
		s.logger.Error("join room failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not join room")
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("roomID")
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" || token == r.Header.Get("Authorization") {
		writeError(w, http.StatusUnauthorized, "missing_token", "use Authorization: Bearer <member_token>")
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Debug("websocket upgrade failed", "error", err)
		return
	}
	c := &client{conn: conn, send: make(chan []byte, 64), done: make(chan struct{})}
	snapshot, err := s.store.Connect(roomID, token, c)
	if err != nil {
		_ = conn.WriteJSON(event{Type: "server.error", Data: map[string]string{"message": "room or member token is invalid"}})
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid room credentials"), time.Now().Add(time.Second))
		c.stop()
		return
	}

	if err := conn.WriteJSON(event{Type: "room.snapshot", Data: snapshot}); err != nil {
		s.store.Disconnect(c)
		c.stop()
		return
	}
	go c.writePump()
	c.readPump()
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		// The response is already committed. There is no useful recovery path;
		// handlers log operational failures before reaching this helper.
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Code: code, Message: message})
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
	})
}
