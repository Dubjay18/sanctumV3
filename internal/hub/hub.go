package hub

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Dubjay/sanctum/pkg/types"
)

type BroadcastMsg struct {
	Envelope *types.Envelope
	Sender   *Client
}

type PresenceUpdateMsg struct {
	UID    string
	Name   string
	RoomID string
	State  types.PresenceState
}

type RoomKeysRequest struct {
	RoomID   string
	Response chan map[string][]byte
}

type Hub struct {
	rooms            map[string]map[*Client]bool
	clients          map[string]*Client
	presence         map[string]types.PresenceState
	pendingDMs       map[string][][]byte
	publicKeys       map[string][]byte
	publicKeysMu     sync.RWMutex
	roomKeysRequests chan RoomKeysRequest
	broadcast        chan BroadcastMsg
	presenceUpdates  chan PresenceUpdateMsg
	register         chan *Client
	unregister       chan *Client
	persistence      chan types.Envelope
	done             chan struct{}
}

func NewHub() *Hub {
	return &Hub{
		rooms:            make(map[string]map[*Client]bool),
		clients:          make(map[string]*Client),
		presence:         make(map[string]types.PresenceState),
		pendingDMs:       make(map[string][][]byte),
		publicKeys:       make(map[string][]byte),
		roomKeysRequests: make(chan RoomKeysRequest),
		broadcast:        make(chan BroadcastMsg, 512),
		presenceUpdates:  make(chan PresenceUpdateMsg, 128),
		register:         make(chan *Client),
		unregister:       make(chan *Client),
		persistence:      make(chan types.Envelope, 1024),
		done:             make(chan struct{}),
	}
}

func (h *Hub) Run() {
	h.RunWithContext(context.Background())
}

func (h *Hub) RunWithContext(ctx context.Context) {
	defer close(h.done)
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-h.roomKeysRequests:
			keys := make(map[string][]byte)
			for client := range h.rooms[req.RoomID] {
				h.publicKeysMu.RLock()
				if pubKey, ok := h.publicKeys[client.ID]; ok {
					keys[client.ID] = pubKey
				}
				h.publicKeysMu.RUnlock()
			}
			req.Response <- keys
		case client := <-h.register:
			roomID := client.RoomID()
			// Remove from any other rooms first
			for otherRoomID, roomClients := range h.rooms {
				if otherRoomID != roomID && roomClients[client] {
					delete(roomClients, client)
					if len(roomClients) == 0 {
						delete(h.rooms, otherRoomID)
					}
					h.broadcastPresence(otherRoomID, types.PresenceUpdate{UID: client.ID, Name: client.Name, State: types.PresenceOffline})
				}
			}

			if roomID != "" {
				if h.rooms[roomID] == nil {
					h.rooms[roomID] = make(map[*Client]bool)
				}
				h.rooms[roomID][client] = true
			}
			h.clients[client.ID] = client
			h.presence[client.ID] = types.PresenceOnline

			clientPubKey := client.GetPublicKey()
			if len(clientPubKey) > 0 {
				h.publicKeysMu.Lock()
				h.publicKeys[client.ID] = clientPubKey
				h.publicKeysMu.Unlock()
			}

			if roomID != "" {
				h.sendPresenceSnapshot(client)
				h.broadcastPresence(roomID, types.PresenceUpdate{UID: client.ID, Name: client.Name, State: types.PresenceOnline})
			}

			if pending, ok := h.pendingDMs[client.ID]; ok {
				for _, data := range pending {
					client.send <- data
				}
				delete(h.pendingDMs, client.ID)
			}

		case client := <-h.unregister:
			if _, ok := h.clients[client.ID]; !ok {
				continue
			}
			roomsWithClient := h.roomsContaining(client)
			for _, roomID := range roomsWithClient {
				if roomClients, ok := h.rooms[roomID]; ok {
					delete(roomClients, client)
					if len(roomClients) == 0 {
						delete(h.rooms, roomID)
					}
				}
			}
			delete(h.clients, client.ID)
			h.presence[client.ID] = types.PresenceOffline
			for _, roomID := range roomsWithClient {
				h.broadcastPresence(roomID, types.PresenceUpdate{UID: client.ID, Name: client.Name, State: types.PresenceOffline})
			}
			h.closeClient(client)

		case update := <-h.presenceUpdates:
			if update.UID == "" {
				continue
			}
			current := h.presence[update.UID]
			if current == update.State {
				continue
			}
			h.presence[update.UID] = update.State
			if update.RoomID != "" {
				h.broadcastPresence(update.RoomID, types.PresenceUpdate{UID: update.UID, Name: update.Name, State: update.State})
			}

		case msg := <-h.broadcast:
			if msg.Envelope == nil {
				continue
			}

			if msg.Envelope.Type == types.TypeText {
				msg.Envelope.Timestamp = time.Now().UnixMilli()
				if msg.Envelope.ID == "" {
					msg.Envelope.ID = uuid.New().String()
				}
				payload, err := types.Marshal(msg.Envelope)
				if err != nil {
					continue
				}

				for roomClient := range h.rooms[msg.Envelope.RoomID] {
					if roomClient == msg.Sender {
						continue
					}
					select {
					case roomClient.send <- payload:
					default:
						roomID := roomClient.RoomID()
						if roomClients, ok := h.rooms[roomID]; ok {
							delete(roomClients, roomClient)
							if len(roomClients) == 0 {
								delete(h.rooms, roomID)
							}
						}
						delete(h.clients, roomClient.ID)
						h.closeClient(roomClient)
					}
				}
				select {
				case h.persistence <- *msg.Envelope:
				default:
				}
				continue
			}

			if msg.Envelope.Type == types.TypeDM {
				if msg.Envelope.ID == "" {
					msg.Envelope.ID = uuid.New().String()
				}
				if msg.Envelope.Timestamp == 0 {
					msg.Envelope.Timestamp = time.Now().UnixMilli()
				}
				payload, err := types.Marshal(msg.Envelope)
				if err != nil {
					continue
				}
				recipient := h.clients[msg.Envelope.ToUID]
				if recipient == nil {
					h.pendingDMs[msg.Envelope.ToUID] = append(h.pendingDMs[msg.Envelope.ToUID], payload)
					errEnv := &types.Envelope{Type: types.TypeError, Payload: "recipient offline", ToUID: msg.Envelope.ToUID}
					if errPayload, err := types.Marshal(errEnv); err == nil {
						select {
						case msg.Sender.send <- errPayload:
						default:
							roomID := msg.Sender.RoomID()
							if roomClients, ok := h.rooms[roomID]; ok {
								delete(roomClients, msg.Sender)
								if len(roomClients) == 0 {
									delete(h.rooms, roomID)
								}
							}
							delete(h.clients, msg.Sender.ID)
							h.closeClient(msg.Sender)
						}
					}
					select {
					case h.persistence <- *msg.Envelope:
					default:
					}
					continue
				}

				select {
				case recipient.send <- payload:
				default:
					roomID := recipient.RoomID()
					if roomClients, ok := h.rooms[roomID]; ok {
						delete(roomClients, recipient)
						if len(roomClients) == 0 {
							delete(h.rooms, roomID)
						}
					}
					delete(h.clients, recipient.ID)
					h.closeClient(recipient)
				}
				select {
				case h.persistence <- *msg.Envelope:
				default:
				}
				continue
			}
		}
	}
}

func (h *Hub) GetPublicKey(uid string) ([]byte, bool) {
	h.publicKeysMu.RLock()
	defer h.publicKeysMu.RUnlock()
	key, ok := h.publicKeys[uid]
	return key, ok
}

func (h *Hub) GetRoomPublicKeys(roomID string) map[string][]byte {
	resp := make(chan map[string][]byte, 1)
	h.roomKeysRequests <- RoomKeysRequest{
		RoomID:   roomID,
		Response: resp,
	}
	return <-resp
}

func (h *Hub) Done() <-chan struct{} {
	return h.done
}

func (h *Hub) broadcastPresence(roomID string, update types.PresenceUpdate) {
	if roomID == "" {
		return
	}
	payload, err := json.Marshal(update)
	if err != nil {
		return
	}
	env := &types.Envelope{
		Type:     types.TypePresenceUpdate,
		RoomID:   roomID,
		FromUID:  update.UID,
		FromName: update.Name,
		Payload:  string(payload),
	}
	data, err := types.Marshal(env)
	if err != nil {
		return
	}

	for roomClient := range h.rooms[roomID] {
		select {
		case roomClient.send <- data:
		default:
			roomID := roomClient.RoomID()
			if roomClients, ok := h.rooms[roomID]; ok {
				delete(roomClients, roomClient)
				if len(roomClients) == 0 {
					delete(h.rooms, roomID)
				}
			}
			delete(h.clients, roomClient.ID)
			h.closeClient(roomClient)
		}
	}
}

func (h *Hub) roomsContaining(client *Client) []string {
	rooms := []string{}
	for roomID, roomClients := range h.rooms {
		if roomClients[client] {
			rooms = append(rooms, roomID)
		}
	}
	return rooms
}

func (h *Hub) sendPresenceSnapshot(client *Client) {
	roomID := client.RoomID()
	if roomID == "" {
		return
	}
	roomClients := h.rooms[roomID]
	if len(roomClients) == 0 {
		return
	}

	snapshot := make([]types.PresenceUpdate, 0, len(roomClients))
	for roomClient := range roomClients {
		state := h.presence[roomClient.ID]
		if state == "" {
			state = types.PresenceOnline
		}
		snapshot = append(snapshot, types.PresenceUpdate{
			UID:   roomClient.ID,
			Name:  roomClient.Name,
			State: state,
		})
	}

	payload, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	env := &types.Envelope{
		Type:    types.TypeHistoryBatch,
		RoomID:  roomID,
		Payload: string(payload),
	}
	data, err := types.Marshal(env)
	if err != nil {
		return
	}

	select {
	case client.send <- data:
	default:
		roomID := client.RoomID()
		if roomClients, ok := h.rooms[roomID]; ok {
			delete(roomClients, client)
			if len(roomClients) == 0 {
				delete(h.rooms, roomID)
			}
		}
		delete(h.clients, client.ID)
		h.closeClient(client)
	}
}

func (h *Hub) closeClient(client *Client) {
	if client.closed {
		return
	}
	client.closed = true
	if client.cancel != nil {
		client.cancel()
	}
	close(client.send)
}

func (h *Hub) SetPersistenceChan(c chan types.Envelope) {
	h.persistence = c
}
