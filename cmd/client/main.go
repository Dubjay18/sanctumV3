package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/joho/godotenv"

	"github.com/Dubjay/sanctum/internal/crypto"
	"github.com/Dubjay/sanctum/internal/tui"
)

func promptPassphrase(prompt string) (string, error) {
	fmt.Print(prompt)
	var pass string
	_, err := fmt.Scan(&pass)
	if err != nil {
		return "", err
	}
	return pass, nil
}

func main() {
	_ = godotenv.Load() // load environment variables for FIREBASE_API_KEY fallback

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("failed to get user home directory: %v", err)
	}
	keysDir := filepath.Join(homeDir, ".sanctum")
	keysPath := filepath.Join(keysDir, "keys.json")

	var pubKey *[32]byte
	var privKey *[32]byte

	if _, err := os.Stat(keysPath); os.IsNotExist(err) {
		// First-launch flow: keys.json does not exist
		if err := os.MkdirAll(keysDir, 0700); err != nil {
			log.Fatalf("failed to create directory %s: %v", keysDir, err)
		}

		var passphrase string
		for {
			p1, err := promptPassphrase("Create passphrase to secure your E2EE keypair: ")
			if err != nil {
				log.Fatalf("failed to read passphrase: %v", err)
			}
			p2, err := promptPassphrase("Confirm passphrase: ")
			if err != nil {
				log.Fatalf("failed to read passphrase confirmation: %v", err)
			}
			if p1 == p2 {
				passphrase = p1
				break
			}
			fmt.Println("Passphrases do not match. Please try again.")
		}

		var genErr error
		pubKey, privKey, genErr = crypto.GenerateKeypair()
		if genErr != nil {
			log.Fatalf("failed to generate keypair: %v", genErr)
		}

		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			log.Fatalf("failed to generate random salt: %v", err)
		}

		encryptedPrivKey, encErr := crypto.EncryptPrivateKey(privKey, []byte(passphrase), salt)
		if encErr != nil {
			log.Fatalf("failed to encrypt private key: %v", encErr)
		}

		encJSON := crypto.EncryptedPrivateKeyJSON{
			Ciphertext: base64.StdEncoding.EncodeToString(encryptedPrivKey),
			Salt:       base64.StdEncoding.EncodeToString(salt),
		}
		encryptedJSONBytes, marshalErr := json.Marshal(encJSON)
		if marshalErr != nil {
			log.Fatalf("failed to marshal encrypted private key JSON: %v", marshalErr)
		}

		if saveErr := crypto.SaveKeypair(pubKey[:], encryptedJSONBytes, keysPath); saveErr != nil {
			log.Fatalf("failed to save keypair: %v", saveErr)
		}

		fmt.Println("Keypair generated and saved successfully to", keysPath)
	} else {
		// Existing-launch flow: keys.json exists
		pubKeyBytes, encryptedJSON, loadErr := crypto.LoadKeypair(keysPath)
		if loadErr != nil {
			log.Fatalf("failed to load keypair: %v", loadErr)
		}

		var pubKeyArr [32]byte
		copy(pubKeyArr[:], pubKeyBytes)
		pubKey = &pubKeyArr

		passphrase, passErr := promptPassphrase("Enter passphrase to decrypt your E2EE keys: ")
		if passErr != nil {
			log.Fatalf("failed to read passphrase: %v", passErr)
		}

		var decErr error
		privKey, decErr = crypto.DecryptPrivateKey(encryptedJSON, []byte(passphrase))
		if decErr != nil {
			log.Fatalf("failed to decrypt private key: %v", decErr)
		}
	}

	wsURL := "ws://localhost:8080/ws"
	
	// Load ServerURL from config if defined
	cfgPath := tui.DefaultConfigPath()
	if cfg, err := tui.LoadConfig(cfgPath); err == nil && cfg.ServerURL != "" {
		wsURL = cfg.ServerURL
	}

	pHolder := &tui.ProgramHolder{}
	model := tui.NewAppModel(wsURL, pubKey, privKey, pHolder)
	program := tea.NewProgram(model, tea.WithAltScreen())
	pHolder.P = program

	if _, err := program.Run(); err != nil {
		log.Fatalf("program exited with error: %v", err)
	}
}
