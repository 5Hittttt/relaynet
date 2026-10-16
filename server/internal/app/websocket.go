package app

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait    = 10 * time.Second
	pongWait     = 60 * time.Second
	pingPeriod   = 50 * time.Second
	maxWSMessage = 8 * 1024
	maxChatBytes = 8 * 1024
)

type client struct {
	conn     *websocket.Conn
	send     chan []byte
	done     chan struct{}
	stopOnce sync.Once
	store    *Store
	roomID   string
	member   Member
}

type clientEvent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (c *client) stop() {
	c.stopOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *client) readPump() {
	defer func() {
		c.store.Disconnect(c)
		c.stop()
	}()

	c.conn.SetReadLimit(maxWSMessage)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		messageType, payload, err := c.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				// Routine disconnects are expected; the connection close is recorded
				// by the HTTP request logger and room presence event.
			}
			return
		}
		if messageType != websocket.TextMessage || len(payload) > maxChatBytes {
			c.sendProtocolError("only text chat events up to 8 KiB are accepted")
			continue
		}

		var incoming clientEvent
		decoder := json.NewDecoder(strings.NewReader(string(payload)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&incoming); err != nil {
			c.sendProtocolError("invalid event JSON")
			continue
		}
		if incoming.Type != "chat.send" {
			c.sendProtocolError("unsupported event type")
			continue
		}
		if err := c.store.PublishChat(c, incoming.Text); err != nil {
			if errors.Is(err, errInvalidChat) {
				c.sendProtocolError("chat text must contain 1 to 2000 characters")
				continue
			}
			return
		}
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.stop()
	}()
	for {
		select {
		case <-c.done:
			return
		case payload := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

func (c *client) sendProtocolError(message string) {
	payload := eventBytes("server.error", map[string]string{"message": message})
	select {
	case c.send <- payload:
	default:
		c.stop()
	}
}
