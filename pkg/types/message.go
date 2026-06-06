package types

import "encoding/json"

type MessageType string

type PresenceState string

const (
	TypeText           MessageType = "text"
	TypeJoinRoom       MessageType = "join_room"
	TypeLeaveRoom      MessageType = "leave_room"
	TypeDM             MessageType = "dm"
	TypePresenceUpdate MessageType = "presence_update"
	TypeError          MessageType = "error"
	TypeAck            MessageType = "ack"
	TypeHistoryBatch   MessageType = "history_batch"
	TypeDeliveredAck   MessageType = "delivered_ack"
	TypeReadAck        MessageType = "read_ack"
	TypeAIMessage      MessageType = "ai_message"
)

const (
	PresenceOnline  PresenceState = "online"
	PresenceAway    PresenceState = "away"
	PresenceOffline PresenceState = "offline"
)

type PresenceUpdate struct {
	UID   string        `json:"uid"`
	Name  string        `json:"name,omitempty"`
	State PresenceState `json:"state"`
}

type EncryptedPayload struct {
	Ciphertext string `json:"ciphertext"`
	Nonce      string `json:"nonce"`
}

type Envelope struct {
	ID                string                      `json:"id"`
	Type              MessageType                 `json:"type"`
	FromUID           string                      `json:"from_uid,omitempty"`
	FromName          string                      `json:"from_name,omitempty"`
	ToUID             string                      `json:"to_uid,omitempty"`
	RoomID            string                      `json:"room_id,omitempty"`
	Nonce             string                      `json:"nonce,omitempty"`
	Payload           string                      `json:"payload,omitempty"`
	Timestamp         int64                       `json:"timestamp,omitempty"`
	DeliveryStatus    string                      `json:"delivery_status,omitempty"`
	EncryptedPayloads map[string]EncryptedPayload `json:"encrypted_payloads,omitempty"`
}

func NewTextEnvelope(from, room, payload string) *Envelope {
	return &Envelope{
		Type:    TypeText,
		FromUID: from,
		RoomID:  room,
		Payload: payload,
	}
}

func NewDMEnvelope(from, to, payload string) *Envelope {
	return &Envelope{
		Type:    TypeDM,
		FromUID: from,
		ToUID:   to,
		Payload: payload,
	}
}

func NewPresenceEnvelope(uid string, state PresenceState) *Envelope {
	return &Envelope{
		Type:    TypePresenceUpdate,
		FromUID: uid,
		Payload: string(state),
	}
}

func Marshal(env *Envelope) ([]byte, error) {
	return json.Marshal(env)
}

func Unmarshal(data []byte) (*Envelope, error) {
	var env Envelope
	err := json.Unmarshal(data, &env)
	if err != nil {
		return nil, err
	}
	return &env, nil
}
