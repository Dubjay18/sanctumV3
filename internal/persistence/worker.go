package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/Dubjay/sanctum/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type FirestoreEncryptedPayload struct {
	Ciphertext string `firestore:"ciphertext"`
	Nonce      string `firestore:"nonce"`
}

type FirestoreRoomMessage struct {
	ID                string                               `firestore:"id"`
	Type              string                               `firestore:"type,omitempty"`
	FromUID           string                               `firestore:"from_uid"`
	FromName          string                               `firestore:"from_name"`
	EncryptedPayloads map[string]FirestoreEncryptedPayload `firestore:"encrypted_payloads"`
	Timestamp         time.Time                            `firestore:"timestamp,serverTimestamp"`
	RoomID            string                               `firestore:"room_id"`
	Nonce             string                               `firestore:"nonce,omitempty"`
	Payload           string                               `firestore:"payload,omitempty"`
}

type FirestoreDMMessage struct {
	ID         string    `firestore:"id"`
	FromUID    string    `firestore:"from_uid"`
	Ciphertext string    `firestore:"ciphertext"`
	Nonce      string    `firestore:"nonce"`
	Timestamp  time.Time `firestore:"timestamp,serverTimestamp"`
}

type Worker struct {
	fsClient *firestore.Client
	queue    chan types.Envelope
	wg       sync.WaitGroup
}

// NewWorker initializes a new persistence Worker with the given Firestore client and buffer size.
func NewWorker(fsClient *firestore.Client, bufferSize int) *Worker {
	return &Worker{
		fsClient: fsClient,
		queue:    make(chan types.Envelope, bufferSize),
	}
}

// Queue returns the channel to send envelopes to the persistence worker.
func (w *Worker) Queue() chan types.Envelope {
	return w.queue
}

// Start kicks off the background goroutine that reads from the queue and persists messages.
func (w *Worker) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env, ok := <-w.queue:
				if !ok {
					return
				}
				w.wg.Add(1)
				go func(e types.Envelope) {
					defer w.wg.Done()
					var err error
					if e.Type == types.TypeText || e.Type == types.TypeAIMessage {
						err = w.persistRoomMessage(ctx, e)
					} else if e.Type == types.TypeDM {
						err = w.persistDMMessage(ctx, e)
					}
					if err != nil {
						envJSON, _ := json.Marshal(e)
						slog.ErrorContext(ctx, "PERSISTENCE_FAILURE: failed to persist envelope after retries", "error", err, "envelope", string(envJSON))
					}
				}(env)
			}
		}
	}()
}

// Wait blocks until all active background persistence writes have completed.
func (w *Worker) Wait() {
	w.wg.Wait()
}

func (w *Worker) persistRoomMessage(ctx context.Context, env types.Envelope) error {
	docID := env.ID
	if docID == "" {
		return fmt.Errorf("envelope ID is empty")
	}
	roomID := env.RoomID
	if roomID == "" {
		return fmt.Errorf("room ID is empty")
	}

	payloads := make(map[string]FirestoreEncryptedPayload)
	for uid, ep := range env.EncryptedPayloads {
		payloads[uid] = FirestoreEncryptedPayload{
			Ciphertext: ep.Ciphertext,
			Nonce:      ep.Nonce,
		}
	}

	dbMsg := FirestoreRoomMessage{
		ID:                docID,
		Type:              string(env.Type),
		FromUID:           env.FromUID,
		FromName:          env.FromName,
		EncryptedPayloads: payloads,
		RoomID:            roomID,
		Nonce:             env.Nonce,
		Payload:           env.Payload,
	}

	ref := w.fsClient.Collection("rooms").Doc(roomID).Collection("messages").Doc(docID)
	return w.setWithRetry(ctx, ref, dbMsg)
}

func (w *Worker) persistDMMessage(ctx context.Context, env types.Envelope) error {
	docID := env.ID
	if docID == "" {
		return fmt.Errorf("envelope ID is empty")
	}
	if env.FromUID == "" || env.ToUID == "" {
		return fmt.Errorf("from_uid or to_uid is empty")
	}

	threadID := sortedJoin(env.FromUID, env.ToUID)

	dbMsg := FirestoreDMMessage{
		ID:         docID,
		FromUID:    env.FromUID,
		Ciphertext: env.Payload,
		Nonce:      env.Nonce,
	}

	ref := w.fsClient.Collection("dms").Doc(threadID).Collection("messages").Doc(docID)
	return w.setWithRetry(ctx, ref, dbMsg)
}

func (w *Worker) setWithRetry(ctx context.Context, ref *firestore.DocumentRef, data interface{}) error {
	backoffs := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
	}

	var err error
	for i := 0; i <= 5; i++ {
		_, err = ref.Set(ctx, data)
		if err == nil {
			return nil
		}

		if st, ok := status.FromError(err); ok {
			code := st.Code()
			if code == codes.PermissionDenied || code == codes.NotFound || code == codes.InvalidArgument {
				return err
			}
		}

		if i < 5 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoffs[i]):
			}
		}
	}
	return err
}

func sortedJoin(uid1, uid2 string) string {
	uids := []string{uid1, uid2}
	sort.Strings(uids)
	return fmt.Sprintf("%s_%s", uids[0], uids[1])
}
