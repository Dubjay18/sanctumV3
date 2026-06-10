package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
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

type DeliveryState int

const (
	StateSent DeliveryState = iota
	StateDelivered
	StateRead
)

type GroupDeliveryTracker struct {
	States        map[string]DeliveryState
	DeliveredSent bool
	ReadSent      bool
}

type RoomKeysRequest struct {
	RoomID   string
	Response chan map[string][]byte
}

type Hub struct {
	rooms            map[string]map[*Client]bool
	clients          map[string]*Client
	clientsMu        sync.RWMutex
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
	FirestoreClient  *firestore.Client
	roomCache        map[string]roomCacheEntry
	roomCacheMu      sync.RWMutex
	messageSenders   map[string]string
	messageSendersMu sync.RWMutex
	groupDelivery    map[string]*GroupDeliveryTracker
	groupDeliveryMu  sync.Mutex
}

type roomCacheEntry struct {
	room      types.Room
	expiresAt time.Time
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
		roomCache:        make(map[string]roomCacheEntry),
		messageSenders:   make(map[string]string),
		groupDelivery:    make(map[string]*GroupDeliveryTracker),
	}
}

func (h *Hub) Run() {
	h.RunWithContext(context.Background())
}

func (h *Hub) RunWithContext(ctx context.Context) {
	defer close(h.done)
	h.startCacheCleaner(ctx)
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
			h.clientsMu.Lock()
			h.clients[client.ID] = client
			h.clientsMu.Unlock()
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
			h.clientsMu.RLock()
			_, exists := h.clients[client.ID]
			h.clientsMu.RUnlock()
			if !exists {
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
			h.clientsMu.Lock()
			delete(h.clients, client.ID)
			h.clientsMu.Unlock()
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

			// Handle DeliveredAck and ReadAck
			if msg.Envelope.Type == types.TypeDeliveredAck || msg.Envelope.Type == types.TypeReadAck {
				msgID := msg.Envelope.ID
				if msgID == "" {
					continue
				}

				h.messageSendersMu.RLock()
				senderUID, ok := h.messageSenders[msgID]
				h.messageSendersMu.RUnlock()
				if !ok {
					senderUID = msg.Envelope.ToUID
				}

				if senderUID == "" {
					continue
				}

				ackSenderUID := msg.Sender.ID
				if ackSenderUID == "" && msg.Envelope.FromUID != "" {
					ackSenderUID = msg.Envelope.FromUID
				}

				if msg.Envelope.RoomID != "" {
					h.groupDeliveryMu.Lock()
					tracker, exists := h.groupDelivery[msgID]
					if exists {
						targetState := StateDelivered
						if msg.Envelope.Type == types.TypeReadAck {
							targetState = StateRead
						}

						if currState, ok := tracker.States[ackSenderUID]; !ok || currState < targetState {
							tracker.States[ackSenderUID] = targetState
						}

						if msg.Envelope.Type == types.TypeDeliveredAck {
							allDelivered := true
							for _, state := range tracker.States {
								if state < StateDelivered {
									allDelivered = false
									break
								}
							}
							if allDelivered && !tracker.DeliveredSent {
								tracker.DeliveredSent = true
								h.sendAckToSender(senderUID, &types.Envelope{
									Type:   types.TypeDeliveredAck,
									ID:     msgID,
									RoomID: msg.Envelope.RoomID,
								})
							}
						} else if msg.Envelope.Type == types.TypeReadAck {
							allRead := true
							for _, state := range tracker.States {
								if state < StateRead {
									allRead = false
									break
								}
							}
							if allRead && !tracker.ReadSent {
								tracker.ReadSent = true
								h.sendAckToSender(senderUID, &types.Envelope{
									Type:   types.TypeReadAck,
									ID:     msgID,
									RoomID: msg.Envelope.RoomID,
								})

								h.messageSendersMu.Lock()
								delete(h.messageSenders, msgID)
								h.messageSendersMu.Unlock()
								delete(h.groupDelivery, msgID)
							}
						}
					}
					h.groupDeliveryMu.Unlock()
				} else {
					h.sendAckToSender(senderUID, &types.Envelope{
						Type:  msg.Envelope.Type,
						ID:    msgID,
						ToUID: ackSenderUID,
					})

					if msg.Envelope.Type == types.TypeReadAck {
						h.messageSendersMu.Lock()
						delete(h.messageSenders, msgID)
						h.messageSendersMu.Unlock()
					}
				}
				continue
			}

			// Pre-fill fields for standard text/DM/AI messages
			if msg.Envelope.Type == types.TypeText || msg.Envelope.Type == types.TypeAIMessage {
				msg.Envelope.Timestamp = time.Now().UnixMilli()
				if msg.Envelope.ID == "" {
					msg.Envelope.ID = uuid.New().String()
				}

				h.messageSendersMu.Lock()
				senderUID := msg.Envelope.FromUID
				if senderUID == "" && msg.Sender != nil {
					senderUID = msg.Sender.ID
				}
				h.messageSenders[msg.Envelope.ID] = senderUID
				h.messageSendersMu.Unlock()
			} else if msg.Envelope.Type == types.TypeDM {
				if msg.Envelope.ID == "" {
					msg.Envelope.ID = uuid.New().String()
				}
				if msg.Envelope.Timestamp == 0 {
					msg.Envelope.Timestamp = time.Now().UnixMilli()
				}

				h.messageSendersMu.Lock()
				senderUID := msg.Envelope.FromUID
				if senderUID == "" && msg.Sender != nil {
					senderUID = msg.Sender.ID
				}
				h.messageSenders[msg.Envelope.ID] = senderUID
				h.messageSendersMu.Unlock()
			}

			if msg.Envelope.Type == types.TypeText || msg.Envelope.Type == types.TypeAIMessage {
				payload, err := types.Marshal(msg.Envelope)
				if err != nil {
					continue
				}

				roomID := msg.Envelope.RoomID
				if roomID != "" && msg.Envelope.ID != "" {
					senderID := msg.Envelope.FromUID
					if senderID == "" && msg.Sender != nil {
						senderID = msg.Sender.ID
					}

					h.groupDeliveryMu.Lock()
					tracker := &GroupDeliveryTracker{
						States: make(map[string]DeliveryState),
					}
					for rc := range h.rooms[roomID] {
						if rc.ID != senderID {
							tracker.States[rc.ID] = StateSent
						}
					}
					h.groupDelivery[msg.Envelope.ID] = tracker
					h.groupDeliveryMu.Unlock()

					go func(rID, mID, sID string) {
						room, err := h.GetCachedRoom(context.Background(), rID)
						if err == nil {
							h.groupDeliveryMu.Lock()
							currTracker, exists := h.groupDelivery[mID]
							if exists {
								for _, memUID := range room.MemberUIDs {
									if memUID != sID {
										if _, ok := currTracker.States[memUID]; !ok {
											currTracker.States[memUID] = StateSent
										}
									}
								}
							}
							h.groupDeliveryMu.Unlock()
						}
					}(roomID, msg.Envelope.ID, senderID)
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
						h.clientsMu.Lock()
						delete(h.clients, roomClient.ID)
						h.clientsMu.Unlock()
						h.closeClient(roomClient)
					}
				}
				select {
				case h.persistence <- *msg.Envelope:
					if msg.Sender != nil && msg.Envelope.ID != "" {
						ackEnv := &types.Envelope{
							Type:   types.TypeAck,
							ID:     msg.Envelope.ID,
							RoomID: msg.Envelope.RoomID,
						}
						ackData, err := types.Marshal(ackEnv)
						if err == nil {
							select {
							case msg.Sender.send <- ackData:
							default:
							}
						}
					}
				default:
				}
				continue
			}

			if msg.Envelope.Type == types.TypeDM {
				payload, err := types.Marshal(msg.Envelope)
				if err != nil {
					continue
				}
				h.clientsMu.RLock()
				recipient := h.clients[msg.Envelope.ToUID]
				h.clientsMu.RUnlock()
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
							h.clientsMu.Lock()
							delete(h.clients, msg.Sender.ID)
							h.clientsMu.Unlock()
							h.closeClient(msg.Sender)
						}
					}
					select {
					case h.persistence <- *msg.Envelope:
						if msg.Sender != nil && msg.Envelope.ID != "" {
							ackEnv := &types.Envelope{
								Type:  types.TypeAck,
								ID:    msg.Envelope.ID,
								ToUID: msg.Envelope.ToUID,
							}
							ackData, err := types.Marshal(ackEnv)
							if err == nil {
								select {
								case msg.Sender.send <- ackData:
								default:
								}
							}
						}
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
					h.clientsMu.Lock()
					delete(h.clients, recipient.ID)
					h.clientsMu.Unlock()
					h.closeClient(recipient)
				}
				select {
				case h.persistence <- *msg.Envelope:
					if msg.Sender != nil && msg.Envelope.ID != "" {
						ackEnv := &types.Envelope{
							Type:  types.TypeAck,
							ID:    msg.Envelope.ID,
							ToUID: msg.Envelope.ToUID,
						}
						ackData, err := types.Marshal(ackEnv)
						if err == nil {
							select {
							case msg.Sender.send <- ackData:
							default:
							}
						}
					}
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
			h.clientsMu.Lock()
			delete(h.clients, roomClient.ID)
			h.clientsMu.Unlock()
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
		h.clientsMu.Lock()
		delete(h.clients, client.ID)
		h.clientsMu.Unlock()
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

func (h *Hub) SetFirestoreClient(fs *firestore.Client) {
	h.FirestoreClient = fs
}

func (h *Hub) GetCachedRoom(ctx context.Context, roomID string) (types.Room, error) {
	h.roomCacheMu.RLock()
	entry, ok := h.roomCache[roomID]
	h.roomCacheMu.RUnlock()

	if ok && time.Now().Before(entry.expiresAt) {
		return entry.room, nil
	}

	if h.FirestoreClient == nil {
		return types.Room{}, fmt.Errorf("firestore client is nil")
	}

	doc, err := h.FirestoreClient.Collection("rooms").Doc(roomID).Get(ctx)
	if err != nil {
		return types.Room{}, err
	}

	var r types.Room
	if err := doc.DataTo(&r); err != nil {
		return types.Room{}, err
	}

	h.roomCacheMu.Lock()
	if h.roomCache == nil {
		h.roomCache = make(map[string]roomCacheEntry)
	}
	h.roomCache[roomID] = roomCacheEntry{
		room:      r,
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	h.roomCacheMu.Unlock()

	return r, nil
}

func (h *Hub) InvalidateRoomCache(roomID string) {
	h.roomCacheMu.Lock()
	if h.roomCache != nil {
		delete(h.roomCache, roomID)
	}
	h.roomCacheMu.Unlock()
}

func (h *Hub) startCacheCleaner(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				h.roomCacheMu.Lock()
				if h.roomCache != nil {
					now := time.Now()
					for rID, entry := range h.roomCache {
						if now.After(entry.expiresAt) {
							delete(h.roomCache, rID)
						}
					}
				}
				h.roomCacheMu.Unlock()
			}
		}
	}()
}

func (h *Hub) GetClient(uid string) (*Client, bool) {
	h.clientsMu.RLock()
	defer h.clientsMu.RUnlock()
	c, ok := h.clients[uid]
	return c, ok
}

func (h *Hub) sendAckToSender(senderUID string, env *types.Envelope) {
	h.clientsMu.RLock()
	client, ok := h.clients[senderUID]
	h.clientsMu.RUnlock()
	if ok && client != nil {
		data, err := types.Marshal(env)
		if err == nil {
			select {
			case client.send <- data:
			default:
			}
		}
	}
}


