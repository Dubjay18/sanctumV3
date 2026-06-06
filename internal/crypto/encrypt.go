package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/nacl/box"

	"github.com/Dubjay/sanctum/pkg/types"
)

// EncryptDM encrypts a DM plaintext using NaCl box.
func EncryptDM(plaintext []byte, senderPrivKey, recipientPubKey *[32]byte) (ciphertext, nonce []byte, err error) {
	var n [24]byte
	if _, err := rand.Read(n[:]); err != nil {
		return nil, nil, fmt.Errorf("failed to generate random nonce: %w", err)
	}

	sealed := box.Seal(nil, plaintext, &n, recipientPubKey, senderPrivKey)
	return sealed, n[:], nil
}

// DecryptDM decrypts a NaCl box encrypted DM ciphertext.
func DecryptDM(ciphertext, nonce []byte, senderPubKey, recipientPrivKey *[32]byte) (plaintext []byte, ok bool) {
	if len(nonce) != 24 {
		return nil, false
	}
	var n [24]byte
	copy(n[:], nonce)

	opened, ok := box.Open(nil, ciphertext, &n, senderPubKey, recipientPrivKey)
	return opened, ok
}

// EncryptGroup encrypts a message separately for each room member (Strategy A).
func EncryptGroup(plaintext []byte, senderPrivKey *[32]byte, recipientPubKeys map[string]*[32]byte) (map[string]types.EncryptedPayload, error) {
	res := make(map[string]types.EncryptedPayload)
	for uid, pubKey := range recipientPubKeys {
		ciphertext, nonce, err := EncryptDM(plaintext, senderPrivKey, pubKey)
		if err != nil {
			return nil, err
		}
		res[uid] = types.EncryptedPayload{
			Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
			Nonce:      base64.StdEncoding.EncodeToString(nonce),
		}
	}
	return res, nil
}
