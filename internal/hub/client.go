package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Dubjay/sanctum/pkg/types"
)

const defaultSendBufferSize = 256

type Client struct {
	ID           string
	Name         string
	conn         *websocket.Conn
	send         chan []byte
	hub          *Hub
	roomID       string
	lastActivity time.Time
	lastPresence types.PresenceState
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
	PublicKey    []byte
}

func NewClient(h *Hub, conn *websocket.Conn, id, name, roomID string) *Client {
	return &Client{
		ID:     id,
		Name:   name,
		conn:   conn,
		send:   make(chan []byte, defaultSendBufferSize),
		hub:    h,
		roomID: roomID,
	}
}

func (c *Client) Start() {
	c.hub.register <- c
	go c.writePump()
	go c.readPump()
}

func (c *Client) ensureContext() {
	if c.ctx != nil {
		return
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
}

func (c *Client) GetPublicKey() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.PublicKey
}

func (c *Client) RoomID() string {
	c.mu.Lock()
	roomID := c.roomID
	c.mu.Unlock()
	return roomID
}

func (c *Client) writePump() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				_ = c.conn.Close()
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				_ = c.conn.Close()
				return
			}
		}
	}
}

func (c *Client) readPump() {
	c.ensureContext()
	c.conn.SetReadLimit(4096)
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	c.mu.Lock()
	c.lastActivity = time.Now()
	c.lastPresence = types.PresenceOnline
	c.mu.Unlock()

	ctx := c.ctx
	readCh := make(chan []byte, 1)
	errCh := make(chan error, 1)

	go c.monitorInactivity(ctx)

	go func() {
		for {
			_, data, err := c.conn.ReadMessage()
			if err != nil {
				select {
				case errCh <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case readCh <- data:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			_ = c.conn.Close()
			return
		case err := <-errCh:
			if err != nil {
				c.hub.unregister <- c
			}
			return
		case data := <-readCh:

			var shouldBroadcast bool
			var roomID string
			c.mu.Lock()
			c.lastActivity = time.Now()
			if c.lastPresence == types.PresenceAway {
				c.lastPresence = types.PresenceOnline
				shouldBroadcast = true
			}
			roomID = c.roomID
			c.mu.Unlock()

			if shouldBroadcast {
				c.hub.presenceUpdates <- PresenceUpdateMsg{
					UID:    c.ID,
					Name:   c.Name,
					RoomID: roomID,
					State:  types.PresenceOnline,
				}
			}

			env, err := types.Unmarshal(data)
			if err != nil {
				continue
			}

			if env.FromUID == "" {
				env.FromUID = c.ID
			}
			if env.FromName == "" {
				env.FromName = c.Name
			}
			if env.RoomID == "" {
				env.RoomID = c.RoomID()
			}

			switch env.Type {
			case types.TypeJoinRoom:
				c.mu.Lock()
				var payloadData struct {
					PublicKey string `json:"public_key"`
				}
				if err := json.Unmarshal([]byte(env.Payload), &payloadData); err == nil {
					if pubKeyBytes, err := base64.StdEncoding.DecodeString(payloadData.PublicKey); err == nil {
						c.PublicKey = pubKeyBytes
					}
				}
				c.roomID = env.RoomID
				c.mu.Unlock()
				c.hub.register <- c
				continue
			case types.TypeLeaveRoom:
				previousRoom := ""
				c.mu.Lock()
				previousRoom = c.roomID
				c.roomID = ""
				c.mu.Unlock()
				ack, err := types.Marshal(&types.Envelope{Type: types.TypeAck, RoomID: previousRoom})
				if err == nil {
					c.send <- ack
				}
				c.hub.register <- c
				continue
			}

			c.hub.broadcast <- BroadcastMsg{Envelope: env, Sender: c}
		}
	}
}

func (c *Client) monitorInactivity(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var shouldBroadcast bool
			var roomID string
			c.mu.Lock()
			roomID = c.roomID
			if roomID != "" && time.Since(c.lastActivity) >= 30*time.Second && c.lastPresence != types.PresenceAway {
				c.lastPresence = types.PresenceAway
				shouldBroadcast = true
			}
			c.mu.Unlock()

			if shouldBroadcast {
				c.hub.presenceUpdates <- PresenceUpdateMsg{
					UID:    c.ID,
					Name:   c.Name,
					RoomID: roomID,
					State:  types.PresenceAway,
				}
			}
		}
	}
}
