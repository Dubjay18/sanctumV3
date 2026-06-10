package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Dubjay/sanctum/internal/firebase"
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
	if c.hub.FirestoreClient != nil {
		go c.fetchRoomsAndHistories()
	}
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
	c.ensureContext()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
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
	defer func() {
		if c.cancel != nil {
			c.cancel()
		}
	}()
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
				select {
				case c.hub.unregister <- c:
				case <-c.hub.Done():
				}
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
				c.ensureContext()
				authorized := true
				var rejectPayload string

				if c.hub.FirestoreClient != nil {
					room, err := c.hub.GetCachedRoom(c.ctx, env.RoomID)
					if err != nil {
						st, _ := status.FromError(err)
						switch st.Code() {
						case codes.NotFound:
							rejectPayload = `{"code":"room_not_found"}`
						case codes.PermissionDenied:
							rejectPayload = `{"code":"not_authorized"}`
						case codes.Unavailable:
							rejectPayload = `{"code":"database_offline"}`
						default:
							rejectPayload = `{"code":"database_offline"}`
						}
						authorized = false
					} else {
						if room.IsPrivate {
							foundMember := false
							for _, mUID := range room.MemberUIDs {
								if mUID == c.ID {
									foundMember = true
									break
								}
							}
							if !foundMember {
								authorized = false
								rejectPayload = `{"code":"not_authorized"}`
							}
						}
					}
				}

				if !authorized {
					if rejectPayload == "" {
						rejectPayload = `{"code":"not_authorized"}`
					}
					rejectEnv := &types.Envelope{
						Type:    types.TypeError,
						Payload: rejectPayload,
					}
					rejectData, _ := types.Marshal(rejectEnv)
					c.send <- rejectData
					continue
				}

				c.mu.Lock()
				var payloadData struct {
					PublicKey string `json:"public_key"`
					LastMsgID string `json:"last_msg_id"`
				}
				if err := json.Unmarshal([]byte(env.Payload), &payloadData); err == nil {
					if pubKeyBytes, err := base64.StdEncoding.DecodeString(payloadData.PublicKey); err == nil {
						c.PublicKey = pubKeyBytes
					}
				}
				c.roomID = env.RoomID
				c.mu.Unlock()
				c.hub.register <- c

				if c.hub.FirestoreClient != nil {
					go func(roomID, lastMsgID string) {
						history, err := firebase.FetchRoomHistory(c.ctx, c.hub.FirestoreClient, roomID, lastMsgID, 50)
						if err != nil {
							slog.ErrorContext(c.ctx, "failed to fetch room history", "roomID", roomID, "error", err)
							return
						}
						if len(history) > 0 {
							historyJSON, err := json.Marshal(history)
							if err != nil {
								slog.ErrorContext(c.ctx, "failed to marshal room history", "roomID", roomID, "error", err)
								return
							}
							batchEnv := &types.Envelope{
								Type:    types.TypeHistoryBatch,
								RoomID:  roomID,
								Payload: string(historyJSON),
							}
							batchData, err := types.Marshal(batchEnv)
							if err != nil {
								slog.ErrorContext(c.ctx, "failed to marshal history batch envelope", "roomID", roomID, "error", err)
								return
							}
							c.send <- batchData
						}
					}(env.RoomID, payloadData.LastMsgID)
				}
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

			case types.TypeCreateRoom:
				roomName := env.Payload
				if roomName == "" {
					continue
				}
				roomID := uuid.New().String()

				if c.hub.FirestoreClient != nil {
					c.ensureContext()
					roomDoc := types.Room{
						ID:         roomID,
						Name:       roomName,
						CreatedBy:  c.ID,
						CreatedAt:  time.Now(),
						MemberUIDs: []string{c.ID},
						IsPrivate:  false,
					}
					_, err := c.hub.FirestoreClient.Collection("rooms").Doc(roomID).Set(c.ctx, roomDoc)
					if err != nil {
						slog.ErrorContext(c.ctx, "failed to persist room metadata", "roomID", roomID, "error", err)
						errEnv := &types.Envelope{
							Type:    types.TypeError,
							Payload: `{"code":"failed_to_create_room"}`,
						}
						errData, _ := types.Marshal(errEnv)
						c.send <- errData
						continue
					}
					c.hub.InvalidateRoomCache(roomID)
				}

				c.mu.Lock()
				c.roomID = roomID
				c.mu.Unlock()
				c.hub.register <- c

				ackEnv := &types.Envelope{
					Type:    types.TypeCreateRoom,
					RoomID:  roomID,
					Payload: roomName,
				}
				ackData, err := types.Marshal(ackEnv)
				if err == nil {
					c.send <- ackData
				}
				continue

			case types.TypeInvite:
				targetUID := env.ToUID
				roomID := env.RoomID
				if targetUID == "" || roomID == "" {
					continue
				}

				c.ensureContext()
				if c.hub.FirestoreClient != nil {
					room, err := c.hub.GetCachedRoom(c.ctx, roomID)
					if err != nil {
						slog.ErrorContext(c.ctx, "invite failed to get room metadata", "roomID", roomID, "error", err)
						errEnv := &types.Envelope{
							Type:    types.TypeError,
							Payload: `{"code":"room_not_found"}`,
						}
						errData, _ := types.Marshal(errEnv)
						c.send <- errData
						continue
					}

					isMember := false
					for _, mUID := range room.MemberUIDs {
						if mUID == c.ID {
							isMember = true
							break
						}
					}
					if !isMember {
						rejectEnv := &types.Envelope{
							Type:    types.TypeError,
							Payload: `{"code":"not_authorized"}`,
						}
						rejectData, _ := types.Marshal(rejectEnv)
						c.send <- rejectData
						continue
					}

					_, err = c.hub.FirestoreClient.Collection("rooms").Doc(roomID).Update(c.ctx, []firestore.Update{
						{
							Path:  "member_uids",
							Value: firestore.ArrayUnion(targetUID),
						},
					})
					if err != nil {
						slog.ErrorContext(c.ctx, "failed to update room member_uids", "roomID", roomID, "targetUID", targetUID, "error", err)
						errEnv := &types.Envelope{
							Type:    types.TypeError,
							Payload: `{"code":"invite_failed"}`,
						}
						errData, _ := types.Marshal(errEnv)
						c.send <- errData
						continue
					}

					c.hub.InvalidateRoomCache(roomID)

					if targetClient, ok := c.hub.GetClient(targetUID); ok {
						inviteNotify := &types.Envelope{
							Type:    types.TypeInvite,
							RoomID:  roomID,
							Payload: room.Name,
						}
						notifyData, err := types.Marshal(inviteNotify)
						if err == nil {
							targetClient.send <- notifyData
						}
					}
				}
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

func (c *Client) fetchRoomsAndHistories() {
	c.ensureContext()
	ctx := c.ctx
	rooms, err := firebase.FetchUserRooms(ctx, c.hub.FirestoreClient, c.ID)
	if err != nil {
		return
	}

	var g errgroup.Group
	g.SetLimit(5)

	for _, r := range rooms {
		rID := r.ID
		g.Go(func() error {
			history, err := firebase.FetchRoomHistory(ctx, c.hub.FirestoreClient, rID, "", 50)
			if err != nil {
				return err
			}
			if len(history) > 0 {
				historyJSON, err := json.Marshal(history)
				if err == nil {
					batchEnv := &types.Envelope{
						Type:    types.TypeHistoryBatch,
						RoomID:  rID,
						Payload: string(historyJSON),
					}
					batchData, err := types.Marshal(batchEnv)
					if err == nil {
						select {
						case c.send <- batchData:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
			}
			return nil
		})
	}
	_ = g.Wait()
}
