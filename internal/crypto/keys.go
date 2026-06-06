package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

type EncryptedPrivateKeyJSON struct {
	Ciphertext string `json:"ciphertext"`
	Salt       string `json:"salt"`
}

func GenerateKeypair() (publicKey, privateKey *[32]byte, err error) {
	publicKey, privateKey, err = box.GenerateKey(rand.Reader)
	return publicKey, privateKey, err
}

func DeriveKey(passphrase, salt []byte) ([]byte, error) {
	return scrypt.Key(passphrase, salt, 32768, 8, 1, 32)
}

func EncryptPrivateKey(privateKey *[32]byte, passphrase, salt []byte) ([]byte, error) {
	key, err := DeriveKey(passphrase, salt)
	if err != nil {
		return nil, err
	}
	var secretKey [32]byte
	copy(secretKey[:], key)
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	encrypted := secretbox.Seal(nonce[:], privateKey[:], &nonce, &secretKey)
	return encrypted, nil
}

func DecryptPrivateKey(encryptedJSON, passphrase []byte) (*[32]byte, error) {
	var encJSON EncryptedPrivateKeyJSON
	if err := json.Unmarshal(encryptedJSON, &encJSON); err != nil {
		return nil, err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encJSON.Ciphertext)
	if err != nil {
		return nil, err
	}

	salt, err := base64.StdEncoding.DecodeString(encJSON.Salt)
	if err != nil {
		return nil, err
	}

	key, err := DeriveKey(passphrase, salt)
	if err != nil {
		return nil, err
	}

	var secretKey [32]byte
	copy(secretKey[:], key)

	if len(ciphertext) < 24 {
		return nil, fmt.Errorf("invalid encrypted private key length")
	}

	var nonce [24]byte
	copy(nonce[:], ciphertext[:24])

	var privateKey [32]byte
	decrypted, ok := secretbox.Open(privateKey[:0], ciphertext[24:], &nonce, &secretKey)
	if !ok {
		return nil, fmt.Errorf("decryption failed: invalid passphrase or corrupted data")
	}

	if len(decrypted) != 32 {
		return nil, fmt.Errorf("decrypted key length is invalid: expected 32, got %d", len(decrypted))
	}

	copy(privateKey[:], decrypted)
	return &privateKey, nil
}

func SaveKeypair(pubKey, encryptedPrivKey []byte, path string) error {
	fileData := struct {
		PublicKey           string          `json:"public_key"`
		EncryptedPrivateKey json.RawMessage `json:"encrypted_private_key"`
	}{
		PublicKey:           base64.StdEncoding.EncodeToString(pubKey),
		EncryptedPrivateKey: json.RawMessage(encryptedPrivKey),
	}

	data, err := json.Marshal(fileData)
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.Mode().Perm() != 0600 {
		return fmt.Errorf("file permissions are not exactly 0600: got %o", info.Mode().Perm())
	}

	return nil
}

func LoadKeypair(path string) (pubKey []byte, encryptedPrivKey []byte, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	var fileData struct {
		PublicKey           string          `json:"public_key"`
		EncryptedPrivateKey json.RawMessage `json:"encrypted_private_key"`
	}

	if err := json.Unmarshal(data, &fileData); err != nil {
		return nil, nil, err
	}

	pubKey, err = base64.StdEncoding.DecodeString(fileData.PublicKey)
	if err != nil {
		return nil, nil, err
	}

	return pubKey, []byte(fileData.EncryptedPrivateKey), nil
}
