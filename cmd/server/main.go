package main

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"

	"github.com/Dubjay/sanctum/internal/hub"

	"github.com/gorilla/websocket"
)

func main() {
	chatHub := hub.NewHub()
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
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for now
			},
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade failed: %v", err)
			return
		}

		log.Printf("client connected from %s", r.RemoteAddr)

		clientName := r.URL.Query().Get("name")
		if clientName == "" {
			clientName = r.RemoteAddr
		}

		clientID := r.URL.Query().Get("id")
		if clientID == "" {
			clientID = clientName
		}

		client := hub.NewClient(chatHub, conn, clientID, clientName, r.URL.Query().Get("room"))
		client.Start()
	})

	http.ListenAndServe(":8080", mux)
}
