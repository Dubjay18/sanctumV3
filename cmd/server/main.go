package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/Dubjay/sanctum/internal/firebase"
	"github.com/Dubjay/sanctum/internal/hub"
	"github.com/Dubjay/sanctum/internal/persistence"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	credentialsPath := os.Getenv("FIREBASE_CREDENTIALS_PATH")
	if credentialsPath == "" {
		credentialsPath = "./secrets/credentials.json"
	}

	log.Printf("Initializing Firebase client using credentials from: %s", credentialsPath)
	fsClient, authClient, err := firebase.InitFirebase(credentialsPath)
	if err != nil {
		log.Fatalf("Failed to initialize Firebase: %v", err)
	}
	defer fsClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chatHub := hub.NewHub()

	worker := persistence.NewWorker(fsClient, 1024)
	chatHub.SetPersistenceChan(worker.Queue())
	worker.Start(ctx)

	go chatHub.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("GET /keys/{uid}", func(w http.ResponseWriter, r *http.Request) {
		uid := r.PathValue("uid")
		if uid == "" {
			http.Error(w, "missing uid", http.StatusBadRequest)
			return
		}

		pubKey, ok := chatHub.GetPublicKey(uid)
		if !ok {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}

		response := struct {
			UID       string `json:"uid"`
			PublicKey string `json:"public_key"`
		}{
			UID:       uid,
			PublicKey: base64.StdEncoding.EncodeToString(pubKey),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("GET /rooms/{roomID}/keys", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("roomID")
		if roomID == "" {
			http.Error(w, "missing roomID", http.StatusBadRequest)
			return
		}

		keysMap := chatHub.GetRoomPublicKeys(roomID)
		responseKeys := make(map[string]string)
		for uid, pubKey := range keysMap {
			responseKeys[uid] = base64.StdEncoding.EncodeToString(pubKey)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responseKeys)
	})
	wsHandler := func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for now
			},
		}

		uid, _ := r.Context().Value(firebase.ContextKeyUID).(string)
		name, _ := r.Context().Value(firebase.ContextKeyName).(string)
		if name == "" {
			name = uid
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}

		log.Printf("client connected: uid=%s, name=%s, remote=%s", uid, name, r.RemoteAddr)

		client := hub.NewClient(chatHub, conn, uid, name, r.URL.Query().Get("room"))
		client.Start()
	}

	mux.HandleFunc("/ws", firebase.AuthMiddleware(authClient, wsHandler))

	http.ListenAndServe(":8080", mux)
}
