package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestKeypairLifecycle(t *testing.T) {
	// 1. Generate keypair
	pubKey, privKey, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("failed to generate keypair: %v", err)
	}

	passphrase := []byte("correct-passphrase")
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatalf("failed to read random salt: %v", err)
	}

	// 2. Encrypt private key
	encryptedPrivKey, err := EncryptPrivateKey(privKey, passphrase, salt)
	if err != nil {
		t.Fatalf("failed to encrypt private key: %v", err)
	}

	encJSON := EncryptedPrivateKeyJSON{
		Ciphertext: base64.StdEncoding.EncodeToString(encryptedPrivKey),
		Salt:       base64.StdEncoding.EncodeToString(salt),
	}
	encryptedJSONBytes, err := json.Marshal(encJSON)
	if err != nil {
		t.Fatalf("failed to marshal encrypted JSON: %v", err)
	}

	// 3. Save keypair to disk
	tmpDir, err := os.MkdirTemp("", "sanctum-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	keysPath := filepath.Join(tmpDir, "keys.json")
	if err := SaveKeypair(pubKey[:], encryptedJSONBytes, keysPath); err != nil {
		t.Fatalf("failed to save keypair: %v", err)
	}

	// Test: file permissions are 0600 after SaveKeypair
	info, err := os.Stat(keysPath)
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected file permissions to be 0600, got %o", info.Mode().Perm())
	}

	// 4. Load keypair from disk
	loadedPubKey, loadedEncJSON, err := LoadKeypair(keysPath)
	if err != nil {
		t.Fatalf("failed to load keypair: %v", err)
	}

	if !bytes.Equal(loadedPubKey, pubKey[:]) {
		t.Errorf("loaded public key does not match original")
	}

	// 5. Decrypt with correct passphrase -> keys match original
	decryptedPrivKey, err := DecryptPrivateKey(loadedEncJSON, passphrase)
	if err != nil {
		t.Fatalf("failed to decrypt private key: %v", err)
	}

	if !bytes.Equal(decryptedPrivKey[:], privKey[:]) {
		t.Errorf("decrypted private key does not match original")
	}

	// Test: wrong passphrase on decrypt -> secretbox.Open returns false -> return error
	wrongPassphrase := []byte("wrong-passphrase")
	_, err = DecryptPrivateKey(loadedEncJSON, wrongPassphrase)
	if err == nil {
		t.Errorf("expected error when decrypting with wrong passphrase, got nil")
	}
}

func TestGenerateKeypair_Entropy(t *testing.T) {
	// Test: different GenerateKeypair() calls produce different keys (entropy test)
	pub1, priv1, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("failed to generate keypair 1: %v", err)
	}

	pub2, priv2, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("failed to generate keypair 2: %v", err)
	}

	if bytes.Equal(pub1[:], pub2[:]) {
		t.Errorf("expected different public keys, got identical values")
	}

	if bytes.Equal(priv1[:], priv2[:]) {
		t.Errorf("expected different private keys, got identical values")
	}
}
