package firebase

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	firestore "cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	auth "firebase.google.com/go/v4/auth"
	"github.com/Dubjay/sanctum/pkg/types"
	"google.golang.org/api/option"
)

type ContextKey string

const (
	ContextKeyUID  ContextKey = "uid"
	ContextKeyName ContextKey = "name"
)

// InitFirebase initializes the Firebase Admin SDK using the credentials JSON at the specified path.
// It returns both a Firestore client and an Auth client.
func InitFirebase(credentialsPath string) (*firestore.Client, *auth.Client, error) {
	if os.Getenv("ENV") == "development" {
		os.Setenv("FIRESTORE_EMULATOR_HOST", "127.0.0.1:8080")
		os.Setenv("FIREBASE_AUTH_EMULATOR_HOST", "127.0.0.1:9099")
	}

	ctx := context.Background()
	opt := option.WithCredentialsFile(credentialsPath)
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		return nil, nil, err
	}
	fsClient, err := app.Firestore(ctx)
	if err != nil {
		return nil, nil, err
	}
	authClient, err := app.Auth(ctx)
	if err != nil {
		fsClient.Close()
		return nil, nil, err
	}
	return fsClient, authClient, nil
}

// AuthMiddleware wraps a handler to authenticate requests via Firebase ID Token (Bearer token in Authorization header).
func AuthMiddleware(authClient *auth.Client, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		token := parts[1]
		decodedToken, err := authClient.VerifyIDToken(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		name, _ := decodedToken.Claims["name"].(string)

		ctx := context.WithValue(r.Context(), ContextKeyUID, decodedToken.UID)
		ctx = context.WithValue(ctx, ContextKeyName, name)

		next(w, r.WithContext(ctx))
	}
}

// FetchRoomHistory queries room history, ordered by timestamp DESC, cursor-based pagination.
// It returns results reversed to ASC (chronological) order.
func FetchRoomHistory(ctx context.Context, fsClient *firestore.Client, roomID, beforeMsgID string, limit int) ([]types.Envelope, error) {
	if fsClient == nil {
		return nil, fmt.Errorf("firestore client is nil")
	}
	col := fsClient.Collection("rooms").Doc(roomID).Collection("messages")
	query := col.OrderBy("timestamp", firestore.Desc).Limit(limit)

	if beforeMsgID != "" {
		beforeDoc, err := col.Doc(beforeMsgID).Get(ctx)
		if err != nil {
			return nil, err
		}
		query = query.StartAfter(beforeDoc)
	}

	iter := query.Documents(ctx)
	docs, err := iter.GetAll()
	if err != nil {
		return nil, err
	}

	envelopes := make([]types.Envelope, 0, len(docs))
	for _, doc := range docs {
		type FirestoreEncryptedPayload struct {
			Ciphertext string `firestore:"ciphertext"`
			Nonce      string `firestore:"nonce"`
		}
		var msg struct {
			ID                string                               `firestore:"id"`
			Type              string                               `firestore:"type,omitempty"`
			FromUID           string                               `firestore:"from_uid"`
			FromName          string                               `firestore:"from_name"`
			EncryptedPayloads map[string]FirestoreEncryptedPayload `firestore:"encrypted_payloads"`
			Timestamp         time.Time                            `firestore:"timestamp"`
			RoomID            string                               `firestore:"room_id"`
			Nonce             string                               `firestore:"nonce,omitempty"`
			Payload           string                               `firestore:"payload,omitempty"`
		}
		if err := doc.DataTo(&msg); err != nil {
			return nil, err
		}

		eps := make(map[string]types.EncryptedPayload)
		for uid, ep := range msg.EncryptedPayloads {
			eps[uid] = types.EncryptedPayload{
				Ciphertext: ep.Ciphertext,
				Nonce:      ep.Nonce,
			}
		}

		msgType := types.TypeText
		if msg.Type != "" {
			msgType = types.MessageType(msg.Type)
		} else if msg.Payload != "" {
			msgType = types.TypeAIMessage
		}

		envelopes = append(envelopes, types.Envelope{
			ID:                msg.ID,
			Type:              msgType,
			FromUID:           msg.FromUID,
			FromName:          msg.FromName,
			RoomID:            msg.RoomID,
			Nonce:             msg.Nonce,
			EncryptedPayloads: eps,
			Payload:           msg.Payload,
			Timestamp:         msg.Timestamp.UnixMilli(),
		})
	}

	// Reverse to chronological (ASC) order
	for i, j := 0, len(envelopes)-1; i < j; i, j = i+1, j-1 {
		envelopes[i], envelopes[j] = envelopes[j], envelopes[i]
	}

	return envelopes, nil
}

// FetchUserRooms queries Firestore rooms that contain the given UID in their member_uids list.
func FetchUserRooms(ctx context.Context, fsClient *firestore.Client, uid string) ([]types.Room, error) {
	if fsClient == nil {
		return nil, fmt.Errorf("firestore client is nil")
	}
	iter := fsClient.Collection("rooms").Where("member_uids", "array-contains", uid).Documents(ctx)
	docs, err := iter.GetAll()
	if err != nil {
		return nil, err
	}

	rooms := make([]types.Room, 0, len(docs))
	for _, doc := range docs {
		var r types.Room
		if err := doc.DataTo(&r); err != nil {
			return nil, err
		}
		rooms = append(rooms, r)
	}
	return rooms, nil
}
