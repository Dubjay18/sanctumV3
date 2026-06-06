package crypto

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestDM_EncryptionDecryption(t *testing.T) {
	// 1. Generate keys for sender (Alice) and recipient (Bob)
	pubAlice, privAlice, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("failed to generate keypair for Alice: %v", err)
	}

	pubBob, privBob, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("failed to generate keypair for Bob: %v", err)
	}

	plaintext := []byte("This is a highly confidential message!")

	// 2. Encrypt DM (Alice encrypts to Bob using Alice's Private Key and Bob's Public Key)
	ciphertext, nonce, err := EncryptDM(plaintext, privAlice, pubBob)
	if err != nil {
		t.Fatalf("failed to encrypt DM: %v", err)
	}

	// 3. Test: decrypt with correct keys -> plaintext matches
	decrypted, ok := DecryptDM(ciphertext, nonce, pubAlice, privBob)
	if !ok {
		t.Fatalf("failed to decrypt DM with correct keys")
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("decrypted plaintext does not match: expected %q, got %q", plaintext, decrypted)
	}

	// 4. Test: decrypt with wrong private key -> ok == false
	_, ok = DecryptDM(ciphertext, nonce, pubAlice, privAlice)
	if ok {
		t.Errorf("expected decryption to fail with wrong private key, but it succeeded")
	}

	// 5. Test: tampered ciphertext -> ok == false (Poly1305 MAC fails)
	tamperedCiphertext := make([]byte, len(ciphertext))
	copy(tamperedCiphertext, ciphertext)
	if len(tamperedCiphertext) > 0 {
		tamperedCiphertext[len(tamperedCiphertext)-1] ^= 0xFF // alter last byte
	}
	_, ok = DecryptDM(tamperedCiphertext, nonce, pubAlice, privBob)
	if ok {
		t.Errorf("expected decryption to fail with tampered ciphertext, but it succeeded")
	}
}

func TestDM_NonceUniqueness(t *testing.T) {
	_, privAlice, _ := GenerateKeypair()
	pubBob, _, _ := GenerateKeypair()

	seenNonces := make(map[string]bool)

	for i := 0; i < 100; i++ {
		_, nonce, err := EncryptDM([]byte("test"), privAlice, pubBob)
		if err != nil {
			t.Fatalf("failed to encrypt: %v", err)
		}

		nonceStr := string(nonce)
		if seenNonces[nonceStr] {
			t.Errorf("duplicate nonce detected at iteration %d", i)
		}
		seenNonces[nonceStr] = true
	}
}

func TestGroup_EncryptionDecryption(t *testing.T) {
	// 1. Generate keys for 3 room members (Alice, Bob, Charlie) and 1 non-member (Eve)
	pubAlice, privAlice, _ := GenerateKeypair()
	pubBob, privBob, _ := GenerateKeypair()
	pubCharlie, privCharlie, _ := GenerateKeypair()
	_, privEve, _ := GenerateKeypair()

	recipientPubKeys := map[string]*[32]byte{
		"alice":   pubAlice,
		"bob":     pubBob,
		"charlie": pubCharlie,
	}

	plaintext := []byte("Secret group message!")

	// 2. Encrypt for all 3 members (Alice sends)
	payloads, err := EncryptGroup(plaintext, privAlice, recipientPubKeys)
	if err != nil {
		t.Fatalf("failed to encrypt group message: %v", err)
	}

	// Assert that all 3 recipients have payloads in the map
	if len(payloads) != 3 {
		t.Errorf("expected 3 encrypted payloads, got %d", len(payloads))
	}

	// 3. Test: each of the 3 members can decrypt
	for uid, privKey := range map[string]*[32]byte{
		"alice":   privAlice,
		"bob":     privBob,
		"charlie": privCharlie,
	} {
		payload, found := payloads[uid]
		if !found {
			t.Errorf("missing payload for member %s", uid)
			continue
		}

		ciphertext, _ := base64.StdEncoding.DecodeString(payload.Ciphertext)
		nonce, _ := base64.StdEncoding.DecodeString(payload.Nonce)

		decrypted, ok := DecryptDM(ciphertext, nonce, pubAlice, privKey)
		if !ok {
			t.Errorf("member %s failed to decrypt payload", uid)
			continue
		}

		if !bytes.Equal(decrypted, plaintext) {
			t.Errorf("decrypted message for %s does not match: expected %q, got %q", uid, plaintext, decrypted)
		}
	}

	// 4. Test: a non-member cannot find their uid in EncryptedPayloads
	if _, found := payloads["eve"]; found {
		t.Errorf("non-member Eve unexpectedly found in payloads map")
	}

	// Check that even if Eve tries to decrypt someone else's payload (e.g. Bob's) with her key, it fails
	bobPayload := payloads["bob"]
	ciphertext, _ := base64.StdEncoding.DecodeString(bobPayload.Ciphertext)
	nonce, _ := base64.StdEncoding.DecodeString(bobPayload.Nonce)
	_, ok := DecryptDM(ciphertext, nonce, pubAlice, privEve)
	if ok {
		t.Errorf("non-member Eve successfully decrypted Bob's payload")
	}

	// 5. Test: adding a new member (Mallory) after message was sent — they cannot decrypt old messages (no forward secrecy)
	pubMallory, privMallory, _ := GenerateKeypair()
	recipientPubKeys["mallory"] = pubMallory

	// Verify that Mallory's UID is not in the already sent payloads map
	if _, found := payloads["mallory"]; found {
		t.Errorf("new member Mallory unexpectedly found in old payloads map")
	}

	// Verify that Mallory cannot decrypt any of the old payloads (e.g. Alice's payload) with her private key
	alicePayload := payloads["alice"]
	ciphertextAlice, _ := base64.StdEncoding.DecodeString(alicePayload.Ciphertext)
	nonceAlice, _ := base64.StdEncoding.DecodeString(alicePayload.Nonce)
	_, ok = DecryptDM(ciphertextAlice, nonceAlice, pubAlice, privMallory)
	if ok {
		t.Errorf("new member Mallory decrypted Alice's old payload (violates no forward secrecy)")
	}
}
