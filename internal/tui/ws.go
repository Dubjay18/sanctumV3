package tui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"

	"github.com/Dubjay/sanctum/internal/crypto"
	"github.com/Dubjay/sanctum/pkg/types"
)

type WSClient struct {
	conn       *websocket.Conn
	Program    *tea.Program
	PrivateKey *[32]byte
	PublicKey  *[32]byte
	keyring    map[string][]byte
	keyringMu  sync.RWMutex
	serverURL  string
	roomID     string
	roomIDMu   sync.RWMutex
}

func Connect(url string) (*WSClient, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}

	return &WSClient{
		conn:      conn,
		keyring:   make(map[string][]byte),
		serverURL: url,
	}, nil
}

func (c *WSClient) JoinRoom(roomID string) error {
	c.roomIDMu.Lock()
	c.roomID = roomID
	c.roomIDMu.Unlock()

	if c.PublicKey == nil {
		return fmt.Errorf("public key not set on client")
	}
	pubKeyBase64 := base64.StdEncoding.EncodeToString(c.PublicKey[:])
	payload := fmt.Sprintf(`{"public_key":"%s"}`, pubKeyBase64)
	env := &types.Envelope{
		Type:    types.TypeJoinRoom,
		RoomID:  roomID,
		Payload: payload,
	}
	data, err := types.Marshal(env)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *WSClient) LeaveRoom(roomID string) error {
	env := &types.Envelope{
		Type:   types.TypeLeaveRoom,
		RoomID: roomID,
	}
	data, err := types.Marshal(env)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *WSClient) FetchRoomKeys(serverURL, roomID string) (map[string][]byte, error) {
	httpURL := serverURL
	if strings.HasPrefix(httpURL, "ws://") {
		httpURL = "http://" + strings.TrimPrefix(httpURL, "ws://")
	} else if strings.HasPrefix(httpURL, "wss://") {
		httpURL = "https://" + strings.TrimPrefix(httpURL, "wss://")
	}
	httpURL = strings.TrimSuffix(httpURL, "/ws")

	urlStr := fmt.Sprintf("%s/rooms/%s/keys", httpURL, url.PathEscape(roomID))

	resp, err := http.Get(urlStr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var resKeys map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&resKeys); err != nil {
		return nil, err
	}

	result := make(map[string][]byte)
	for uid, base64Key := range resKeys {
		pubKeyBytes, err := base64.StdEncoding.DecodeString(base64Key)
		if err != nil {
			return nil, err
		}
		result[uid] = pubKeyBytes

		c.keyringMu.Lock()
		c.keyring[uid] = pubKeyBytes
		c.keyringMu.Unlock()
	}

	return result, nil
}

func (c *WSClient) FetchPublicKey(serverURL, uid string) ([]byte, error) {
	c.keyringMu.RLock()
	if key, ok := c.keyring[uid]; ok {
		c.keyringMu.RUnlock()
		return key, nil
	}
	c.keyringMu.RUnlock()

	httpURL := serverURL
	if strings.HasPrefix(httpURL, "ws://") {
		httpURL = "http://" + strings.TrimPrefix(httpURL, "ws://")
	} else if strings.HasPrefix(httpURL, "wss://") {
		httpURL = "https://" + strings.TrimPrefix(httpURL, "wss://")
	}
	httpURL = strings.TrimSuffix(httpURL, "/ws")

	urlStr := fmt.Sprintf("%s/keys/%s", httpURL, url.PathEscape(uid))

	resp, err := http.Get(urlStr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var res struct {
		UID       string `json:"uid"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	pubKeyBytes, err := base64.StdEncoding.DecodeString(res.PublicKey)
	if err != nil {
		return nil, err
	}

	c.keyringMu.Lock()
	c.keyring[uid] = pubKeyBytes
	c.keyringMu.Unlock()

	return pubKeyBytes, nil
}

func (c *WSClient) SendDM(recipientUID string, text string) error {
	// Fetch recipient's public key
	pubKeyBytes, err := c.FetchPublicKey(c.serverURL, recipientUID)
	if err != nil {
		return fmt.Errorf("failed to fetch public key for %s: %w", recipientUID, err)
	}

	var theirPubKey [32]byte
	copy(theirPubKey[:], pubKeyBytes)

	// Encrypt the DM
	ciphertext, nonce, err := crypto.EncryptDM([]byte(text), c.PrivateKey, &theirPubKey)
	if err != nil {
		return fmt.Errorf("failed to encrypt DM: %w", err)
	}

	env := &types.Envelope{
		Type:    types.TypeDM,
		ToUID:   recipientUID,
		Payload: base64.StdEncoding.EncodeToString(ciphertext),
		Nonce:   base64.StdEncoding.EncodeToString(nonce),
	}

	data, err := types.Marshal(env)
	if err != nil {
		return err
	}

	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *WSClient) Send(text string) error {
	c.roomIDMu.RLock()
	roomID := c.roomID
	c.roomIDMu.RUnlock()

	var encryptedPayloads map[string]types.EncryptedPayload
	if roomID != "" {
		keys, err := c.FetchRoomKeys(c.serverURL, roomID)
		if err == nil && len(keys) > 0 {
			pubKeys := make(map[string]*[32]byte)
			for uid, keyBytes := range keys {
				var keyArr [32]byte
				copy(keyArr[:], keyBytes)
				pubKeys[uid] = &keyArr
			}

			encPayloads, err := crypto.EncryptGroup([]byte(text), c.PrivateKey, pubKeys)
			if err == nil {
				encryptedPayloads = encPayloads
			}
		}
	}

	env := &types.Envelope{
		Type:              types.TypeText,
		Payload:           text,
		RoomID:            roomID,
		EncryptedPayloads: encryptedPayloads,
	}
	if len(encryptedPayloads) > 0 {
		env.Payload = ""
	}
	data, err := types.Marshal(env)
	if err != nil {
		return err
	}

	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *WSClient) Listen() {
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if c.Program != nil {
				c.Program.Send(WsDisconnectedMsg{})
			}
			return
		}
		if c.Program != nil {
			c.Program.Send(WsMessageMsg{Data: data})
		}
	}
}
