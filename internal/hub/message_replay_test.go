package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Dubjay/sanctum/internal/crypto"
	"github.com/Dubjay/sanctum/internal/firebase"
)

func TestIntegration_MessagePersistAndReplay(t *testing.T) {
	// Ensure we use the emulator
	t.Setenv("ENV", "development")

	credentialsPath := "../../secrets/credentials.json"
	fsClient, _, err := firebase.InitFirebase(credentialsPath)
	if err != nil {
		t.Fatalf("failed to initialize firebase: %v", err)
	}
	defer fsClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Generate keypairs for Alice and Bob
	pubAlice, privAlice, _ := crypto.GenerateKeypair()
	pubBob, privBob, _ := crypto.GenerateKeypair()

	// Setup a room
	roomID := "replay-room-" + uuid.New().String()
	roomDoc := map[string]interface{}{
		"id":          roomID,
		"name":        "Replay Room",
		"created_by":  "alice",
		"created_at":  time.Now(),
		"member_uids": []string{"alice", "bob"},
		"is_private":  true,
	}
	_, err = fsClient.Collection("rooms").Doc(roomID).Set(ctx, roomDoc)
	if err != nil {
		t.Fatalf("failed to create room in Firestore: %v", err)
	}
	defer func() {
		_, _ = fsClient.Collection("rooms").Doc(roomID).Delete(ctx)
	}()

	messagesColl := fsClient.Collection("rooms").Doc(roomID).Collection("messages")

	// 2. Write 60 messages to Firestore
	baseTime := time.Now().Add(-2 * time.Hour)
	recipientPubKeys := map[string]*[32]byte{
		"bob": pubBob,
	}

	for i := 0; i < 60; i++ {
		msgID := fmt.Sprintf("msg-%03d", i)
		plaintext := []byte(fmt.Sprintf("secret message %d", i))
		eps, err := crypto.EncryptGroup(plaintext, privAlice, recipientPubKeys)
		if err != nil {
			t.Fatalf("failed to encrypt message %d: %v", i, err)
		}

		docEPs := make(map[string]interface{})
		for uid, ep := range eps {
			docEPs[uid] = map[string]interface{}{
				"ciphertext": ep.Ciphertext,
				"nonce":      ep.Nonce,
			}
		}

		msg := map[string]interface{}{
			"id":                 msgID,
			"from_uid":           "alice",
			"from_name":          "Alice",
			"timestamp":          baseTime.Add(time.Duration(i) * time.Minute),
			"room_id":            roomID,
			"encrypted_payloads": docEPs,
		}

		_, err = messagesColl.Doc(msgID).Set(ctx, msg)
		if err != nil {
			t.Fatalf("failed to write message %d: %v", i, err)
		}
	}
	defer func() {
		docs, _ := messagesColl.Documents(ctx).GetAll()
		for _, d := range docs {
			_, _ = d.Ref.Delete(ctx)
		}
	}()

	// 3. Call FetchRoomHistory(roomID, "", 50) — verify returns exactly 50, ordered correctly
	history1, err := firebase.FetchRoomHistory(ctx, fsClient, roomID, "", 50)
	if err != nil {
		t.Fatalf("FetchRoomHistory batch 1 failed: %v", err)
	}

	if len(history1) != 50 {
		t.Fatalf("expected exactly 50 messages, got %d", len(history1))
	}

	// Chronological check: the first item in returned slice should be msg-010 (index 10) and last should be msg-059 (index 59)
	if history1[0].ID != "msg-010" {
		t.Errorf("expected first message to be msg-010, got %s", history1[0].ID)
	}
	if history1[49].ID != "msg-059" {
		t.Errorf("expected last message to be msg-059, got %s", history1[49].ID)
	}

	// 4. Nonce-in-Replay Audit: verify Nonce field is non-empty and decrypt succeeds
	for i, env := range history1 {
		msgIdx := i + 10 // msg-010 to msg-059
		ep, found := env.EncryptedPayloads["bob"]
		if !found {
			t.Fatalf("Bob's encrypted payload not found in message %s", env.ID)
		}

		if ep.Nonce == "" {
			t.Errorf("Nonce field is empty for message %s", env.ID)
		}
		if ep.Ciphertext == "" {
			t.Errorf("Ciphertext field is empty for message %s", env.ID)
		}

		// Decrypt and verify
		ciphertext, err1 := base64.StdEncoding.DecodeString(ep.Ciphertext)
		nonce, err2 := base64.StdEncoding.DecodeString(ep.Nonce)
		if err1 != nil || err2 != nil {
			t.Fatalf("failed to base64 decode message %s: %v %v", env.ID, err1, err2)
		}

		decrypted, ok := crypto.DecryptDM(ciphertext, nonce, pubAlice, privBob)
		if !ok {
			t.Fatalf("failed to decrypt message %s", env.ID)
		}

		expectedText := fmt.Sprintf("secret message %d", msgIdx)
		if !bytes.Equal(decrypted, []byte(expectedText)) {
			t.Errorf("decrypted text mismatch for %s: expected %q, got %q", env.ID, expectedText, decrypted)
		}
	}

	// 5. Call again with beforeMsgID = the oldest returned (msg-010) — verify returns next 10
	history2, err := firebase.FetchRoomHistory(ctx, fsClient, roomID, "msg-010", 50)
	if err != nil {
		t.Fatalf("FetchRoomHistory batch 2 failed: %v", err)
	}

	if len(history2) != 10 {
		t.Fatalf("expected exactly 10 messages in batch 2, got %d", len(history2))
	}

	// Chronological check for batch 2: msg-000 to msg-009
	if history2[0].ID != "msg-000" {
		t.Errorf("expected first message of batch 2 to be msg-000, got %s", history2[0].ID)
	}
	if history2[9].ID != "msg-009" {
		t.Errorf("expected last message of batch 2 to be msg-009, got %s", history2[9].ID)
	}

	// Nonce and decryption audit for batch 2
	for i, env := range history2 {
		msgIdx := i // msg-000 to msg-009
		ep, found := env.EncryptedPayloads["bob"]
		if !found {
			t.Fatalf("Bob's encrypted payload not found in batch 2 message %s", env.ID)
		}

		if ep.Nonce == "" {
			t.Errorf("Nonce field is empty for batch 2 message %s", env.ID)
		}

		ciphertext, _ := base64.StdEncoding.DecodeString(ep.Ciphertext)
		nonce, _ := base64.StdEncoding.DecodeString(ep.Nonce)

		decrypted, ok := crypto.DecryptDM(ciphertext, nonce, pubAlice, privBob)
		if !ok {
			t.Fatalf("failed to decrypt batch 2 message %s", env.ID)
		}

		expectedText := fmt.Sprintf("secret message %d", msgIdx)
		if !bytes.Equal(decrypted, []byte(expectedText)) {
			t.Errorf("decrypted text mismatch for batch 2 %s: expected %q, got %q", env.ID, expectedText, decrypted)
		}
	}
}
