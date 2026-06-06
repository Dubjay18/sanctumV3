package hub

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/Dubjay/sanctum/internal/crypto"
	"github.com/Dubjay/sanctum/pkg/types"
)

func TestIntegration_DM_Encryption(t *testing.T) {
	hub, stopHub := startHub(t)
	defer stopHub()

	// 1. Generate keypairs
	pubAlice, privAlice, _ := crypto.GenerateKeypair()
	pubBob, privBob, _ := crypto.GenerateKeypair()

	// 2. Create clients
	alice, cleanupAlice := newTestClient(t, hub, "alice", "Alice", "general")
	defer cleanupAlice()
	bob, cleanupBob := newTestClient(t, hub, "bob", "Bob", "general")
	defer cleanupBob()

	// Populate keys on clients
	alice.mu.Lock()
	alice.PublicKey = pubAlice[:]
	alice.mu.Unlock()

	bob.mu.Lock()
	bob.PublicKey = pubBob[:]
	bob.mu.Unlock()

	// Register clients
	hub.register <- alice
	hub.register <- bob

	// Wait for registration and drain snapshots
	readWithTimeout(t, alice.send, 200*time.Millisecond)
	readWithTimeout(t, bob.send, 200*time.Millisecond)
	drainWithTimeout(alice.send, 50*time.Millisecond)
	drainWithTimeout(bob.send, 50*time.Millisecond)

	// Verify public keys are registered on Hub
	keyAlice, ok := hub.GetPublicKey("alice")
	if !ok || !bytes.Equal(keyAlice, pubAlice[:]) {
		t.Fatalf("Alice's public key not registered correctly")
	}
	keyBob, ok := hub.GetPublicKey("bob")
	if !ok || !bytes.Equal(keyBob, pubBob[:]) {
		t.Fatalf("Bob's public key not registered correctly")
	}

	// 3. Alice encrypts a DM to Bob
	plaintext := []byte("confidential DM message")
	var bobPubKey [32]byte
	copy(bobPubKey[:], keyBob)

	ciphertext, nonce, err := crypto.EncryptDM(plaintext, privAlice, &bobPubKey)
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	dmEnv := &types.Envelope{
		Type:    types.TypeDM,
		FromUID: "alice",
		ToUID:   "bob",
		Payload: base64.StdEncoding.EncodeToString(ciphertext),
		Nonce:   base64.StdEncoding.EncodeToString(nonce),
	}

	// Send it to the Hub
	hub.broadcast <- BroadcastMsg{Envelope: dmEnv, Sender: alice}

	// 4. Bob receives it and decrypts
	msgBytes := readWithTimeout(t, bob.send, 200*time.Millisecond)
	recvEnv, err := types.Unmarshal(msgBytes)
	if err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if recvEnv.Type != types.TypeDM {
		t.Fatalf("expected DM message, got %q", recvEnv.Type)
	}

	// Decrypt
	recvCiphertext, err1 := base64.StdEncoding.DecodeString(recvEnv.Payload)
	recvNonce, err2 := base64.StdEncoding.DecodeString(recvEnv.Nonce)
	if err1 != nil || err2 != nil {
		t.Fatalf("failed to decode ciphertext/nonce: %v %v", err1, err2)
	}

	var alicePubKey [32]byte
	copy(alicePubKey[:], keyAlice)

	decrypted, ok := crypto.DecryptDM(recvCiphertext, recvNonce, &alicePubKey, privBob)
	if !ok {
		t.Fatalf("failed to decrypt Bob's message")
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted plaintext does not match: expected %q, got %q", plaintext, decrypted)
	}
}

func TestIntegration_Group_Encryption(t *testing.T) {
	hub, stopHub := startHub(t)
	defer stopHub()

	// 1. Generate keypairs
	pubAlice, privAlice, _ := crypto.GenerateKeypair()
	pubBob, privBob, _ := crypto.GenerateKeypair()
	pubCharlie, privCharlie, _ := crypto.GenerateKeypair()
	pubEve, privEve, _ := crypto.GenerateKeypair()

	// 2. Create clients: Alice, Bob, Charlie in "general", Eve in "lounge"
	alice, cleanupAlice := newTestClient(t, hub, "alice", "Alice", "general")
	defer cleanupAlice()
	bob, cleanupBob := newTestClient(t, hub, "bob", "Bob", "general")
	defer cleanupBob()
	charlie, cleanupCharlie := newTestClient(t, hub, "charlie", "Charlie", "general")
	defer cleanupCharlie()
	eve, cleanupEve := newTestClient(t, hub, "eve", "Eve", "lounge")
	defer cleanupEve()

	// Set public keys on clients
	alice.mu.Lock()
	alice.PublicKey = pubAlice[:]
	alice.mu.Unlock()

	bob.mu.Lock()
	bob.PublicKey = pubBob[:]
	bob.mu.Unlock()

	charlie.mu.Lock()
	charlie.PublicKey = pubCharlie[:]
	charlie.mu.Unlock()

	eve.mu.Lock()
	eve.PublicKey = pubEve[:]
	eve.mu.Unlock()

	// Register them
	hub.register <- alice
	hub.register <- bob
	hub.register <- charlie
	hub.register <- eve

	// Drain registration messages
	readWithTimeout(t, alice.send, 200*time.Millisecond)
	readWithTimeout(t, bob.send, 200*time.Millisecond)
	readWithTimeout(t, charlie.send, 200*time.Millisecond)
	readWithTimeout(t, eve.send, 200*time.Millisecond)
	drainWithTimeout(alice.send, 50*time.Millisecond)
	drainWithTimeout(bob.send, 50*time.Millisecond)
	drainWithTimeout(charlie.send, 50*time.Millisecond)
	drainWithTimeout(eve.send, 50*time.Millisecond)

	// Fetch keys for the room general
	keysMap := hub.GetRoomPublicKeys("general")
	if len(keysMap) != 3 {
		t.Fatalf("expected 3 room keys, got %d", len(keysMap))
	}

	// 3. Alice encrypts group message
	plaintext := []byte("secret general message")
	recipientPubKeys := make(map[string]*[32]byte)
	for uid, keyBytes := range keysMap {
		var keyArr [32]byte
		copy(keyArr[:], keyBytes)
		recipientPubKeys[uid] = &keyArr
	}

	encryptedPayloads, err := crypto.EncryptGroup(plaintext, privAlice, recipientPubKeys)
	if err != nil {
		t.Fatalf("failed to encrypt group: %v", err)
	}

	groupEnv := &types.Envelope{
		Type:              types.TypeText,
		FromUID:           "alice",
		RoomID:            "general",
		Payload:           "",
		EncryptedPayloads: encryptedPayloads,
	}

	// Send to Hub
	hub.broadcast <- BroadcastMsg{Envelope: groupEnv, Sender: alice}

	// 4. Bob and Charlie receive and decrypt
	for _, member := range []*Client{bob, charlie} {
		msgBytes := readWithTimeout(t, member.send, 200*time.Millisecond)
		recvEnv, err := types.Unmarshal(msgBytes)
		if err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}

		// Look up in EncryptedPayloads
		ep, found := recvEnv.EncryptedPayloads[member.ID]
		if !found {
			t.Fatalf("member %s not found in payloads", member.ID)
		}

		ciphertext, _ := base64.StdEncoding.DecodeString(ep.Ciphertext)
		nonce, _ := base64.StdEncoding.DecodeString(ep.Nonce)

		var alicePubKey [32]byte
		copy(alicePubKey[:], pubAlice[:])

		var memberPrivKey *[32]byte
		if member.ID == "bob" {
			memberPrivKey = privBob
		} else {
			memberPrivKey = privCharlie
		}

		decrypted, ok := crypto.DecryptDM(ciphertext, nonce, &alicePubKey, memberPrivKey)
		if !ok {
			t.Fatalf("member %s failed to decrypt", member.ID)
		}

		if !bytes.Equal(decrypted, plaintext) {
			t.Errorf("member %s decrypted plaintext mismatch", member.ID)
		}
	}

	// 5. Eve does not receive the message
	assertNoMessage(t, eve.send, 100*time.Millisecond)

	// Verify that Eve cannot decrypt someone else's payload (e.g., Bob's)
	bobEp := encryptedPayloads["bob"]
	ciphertext, _ := base64.StdEncoding.DecodeString(bobEp.Ciphertext)
	nonce, _ := base64.StdEncoding.DecodeString(bobEp.Nonce)
	var alicePubKey [32]byte
	copy(alicePubKey[:], pubAlice[:])

	_, ok := crypto.DecryptDM(ciphertext, nonce, &alicePubKey, privEve)
	if ok {
		t.Errorf("non-member Eve decrypted Bob's payload using her private key")
	}
}

func TestAudit_NonceOnReplay(t *testing.T) {
	pubAlice, privAlice, _ := crypto.GenerateKeypair()
	pubBob, privBob, _ := crypto.GenerateKeypair()

	// 1. Simulate DM persistence
	plaintext := []byte("audit DM text")
	ciphertext, nonce, err := crypto.EncryptDM(plaintext, privAlice, pubBob)
	if err != nil {
		t.Fatalf("failed to encrypt: %v", err)
	}

	dmEnv := &types.Envelope{
		Type:    types.TypeDM,
		FromUID: "alice",
		ToUID:   "bob",
		Payload: base64.StdEncoding.EncodeToString(ciphertext),
		Nonce:   base64.StdEncoding.EncodeToString(nonce),
	}

	// Serialize
	data, err := types.Marshal(dmEnv)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Deserialize
	replayedEnv, err := types.Unmarshal(data)
	if err != nil {
		t.Fatalf("failed to unmarshal replayed: %v", err)
	}

	// Verify Nonce is present
	if replayedEnv.Nonce == "" {
		t.Errorf("Nonce field lost on replay")
	}

	// Verify decryption succeeds
	recvCiphertext, _ := base64.StdEncoding.DecodeString(replayedEnv.Payload)
	recvNonce, _ := base64.StdEncoding.DecodeString(replayedEnv.Nonce)
	var alicePubKey [32]byte
	copy(alicePubKey[:], pubAlice[:])

	decrypted, ok := crypto.DecryptDM(recvCiphertext, recvNonce, &alicePubKey, privBob)
	if !ok {
		t.Fatalf("failed to decrypt replayed DM")
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted replayed plaintext mismatch")
	}

	// 2. Simulate Group Message persistence
	groupPlaintext := []byte("audit group text")
	recipients := map[string]*[32]byte{
		"bob": pubBob,
	}
	encryptedPayloads, err := crypto.EncryptGroup(groupPlaintext, privAlice, recipients)
	if err != nil {
		t.Fatalf("failed to encrypt group: %v", err)
	}

	groupEnv := &types.Envelope{
		Type:              types.TypeText,
		FromUID:           "alice",
		RoomID:            "general",
		EncryptedPayloads: encryptedPayloads,
	}

	// Serialize
	groupData, err := types.Marshal(groupEnv)
	if err != nil {
		t.Fatalf("failed to marshal group: %v", err)
	}

	// Deserialize
	replayedGroupEnv, err := types.Unmarshal(groupData)
	if err != nil {
		t.Fatalf("failed to unmarshal replayed group: %v", err)
	}

	// Verify payloads and nonces are present in map
	ep, found := replayedGroupEnv.EncryptedPayloads["bob"]
	if !found {
		t.Fatalf("Bob's payload lost on replay")
	}
	if ep.Nonce == "" || ep.Ciphertext == "" {
		t.Fatalf("Bob's ep fields lost on replay")
	}

	// Decrypt
	recvGroupCiphertext, _ := base64.StdEncoding.DecodeString(ep.Ciphertext)
	recvGroupNonce, _ := base64.StdEncoding.DecodeString(ep.Nonce)

	decryptedGroup, ok := crypto.DecryptDM(recvGroupCiphertext, recvGroupNonce, &alicePubKey, privBob)
	if !ok {
		t.Fatalf("failed to decrypt replayed group message")
	}
	if !bytes.Equal(decryptedGroup, groupPlaintext) {
		t.Errorf("decrypted replayed group plaintext mismatch")
	}
}
