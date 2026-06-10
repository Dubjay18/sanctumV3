package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/Dubjay/sanctum/internal/firebase"
	"github.com/Dubjay/sanctum/internal/hub"
	"github.com/Dubjay/sanctum/internal/logging"
	"github.com/Dubjay/sanctum/internal/persistence"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

func main() {
	baseHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(logging.NewRedactingHandler(baseHandler)))

	_ = godotenv.Load()
	credentialsPath := os.Getenv("FIREBASE_CREDENTIALS_PATH")
	if credentialsPath == "" {
		credentialsPath = "./secrets/credentials.json"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slog.InfoContext(ctx, "Initializing Firebase client", slog.String("credentials_path", credentialsPath))
	fsClient, authClient, err := firebase.InitFirebase(credentialsPath)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to initialize Firebase", slog.Any("error", err))
		os.Exit(1)
	}
	defer fsClient.Close()

	chatHub := hub.NewHub()
	chatHub.SetFirestoreClient(fsClient)

	worker := persistence.NewWorker(fsClient, 1024)
	chatHub.SetPersistenceChan(worker.Queue())
	worker.Start(ctx)

	go chatHub.Run()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("GET /users/{uid}/rooms", firebase.AuthMiddleware(authClient, func(w http.ResponseWriter, r *http.Request) {
		uid := r.PathValue("uid")
		if uid == "" {
			http.Error(w, "missing uid", http.StatusBadRequest)
			return
		}

		tokenUID, _ := r.Context().Value(firebase.ContextKeyUID).(string)
		if tokenUID != uid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		rooms, err := firebase.FetchUserRooms(r.Context(), fsClient, uid)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rooms)
	}))
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
			slog.WarnContext(r.Context(), "websocket upgrade failed", slog.Any("error", err))
			return
		}

		slog.InfoContext(r.Context(), "client connected",
			slog.String("uid", uid),
			slog.String("name", name),
			slog.String("remote", r.RemoteAddr),
		)

		client := hub.NewClient(chatHub, conn, uid, name, r.URL.Query().Get("room"))
		client.Start()
	}

	mux.HandleFunc("/ws", firebase.AuthMiddleware(authClient, wsHandler))

	http.ListenAndServe(":8080", mux)
}
