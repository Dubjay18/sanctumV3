package hub

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Dubjay/sanctum/internal/firebase"
	"github.com/Dubjay/sanctum/internal/persistence"
	"github.com/Dubjay/sanctum/pkg/types"
)

func TestPersistence_BurstLoad(t *testing.T) {
	credentialsPath := "../../secrets/credentials.json"
	fsClient, _, err := firebase.InitFirebase(credentialsPath)
	if err != nil {
		t.Fatalf("failed to initialize firebase: %v", err)
	}
	defer fsClient.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize Hub
	chatHub := NewHub()
	go chatHub.RunWithContext(ctx)

	// Initialize Worker (queue size 1024)
	worker := persistence.NewWorker(fsClient, 1024)
	chatHub.SetPersistenceChan(worker.Queue())
	worker.Start(ctx)

	// Setup a room and delete any leftover messages first to ensure a clean count
	roomID := "burst-room"
	messagesColl := fsClient.Collection("rooms").Doc(roomID).Collection("messages")
	docs, err := messagesColl.Documents(ctx).GetAll()
	if err == nil && len(docs) > 0 {
		batch := fsClient.Batch()
		for _, doc := range docs {
			batch.Delete(doc.Ref)
		}
		_, _ = batch.Commit(ctx)
	}

	numMessages := 500

	t.Logf("Broadcasting %d messages as fast as possible...", numMessages)
	startTime := time.Now()

	// Send 500 messages
	for i := 0; i < numMessages; i++ {
		env := &types.Envelope{
			ID:       fmt.Sprintf("burst-msg-%d", i),
			Type:     types.TypeText,
			RoomID:   roomID,
			FromUID:  "sender-uid",
			FromName: "Sender Name",
			Payload:  fmt.Sprintf("burst payload %d", i),
		}
		chatHub.broadcast <- BroadcastMsg{Envelope: env}
	}

	broadcastDuration := time.Since(startTime)
	t.Logf("Finished enqueueing broadcast to Hub in %v", broadcastDuration)

	if broadcastDuration > 500*time.Millisecond {
		t.Errorf("Hub broadcast queueing was delayed: took %v, expected <500ms", broadcastDuration)
	}

	// Verify all 500 messages appear eventually in Firestore
	t.Log("Verifying messages appear in Firestore...")
	var finalDocsCount int
	var queryErr error

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		docs, err := messagesColl.Documents(ctx).GetAll()
		if err == nil {
			finalDocsCount = len(docs)
			if finalDocsCount == numMessages {
				break
			}
		} else {
			queryErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}

	if queryErr != nil {
		t.Errorf("failed to query Firestore messages: %v", queryErr)
	}

	if finalDocsCount != numMessages {
		t.Fatalf("expected %d messages in Firestore, got %d", numMessages, finalDocsCount)
	}
	t.Logf("Successfully verified %d messages are persisted in Firestore.", finalDocsCount)

	// Verify one message's fields
	docRef := messagesColl.Doc("burst-msg-250")
	snap, err := docRef.Get(ctx)
	if err != nil {
		t.Fatalf("failed to read specific message: %v", err)
	}
	var msg persistence.FirestoreRoomMessage
	if err := snap.DataTo(&msg); err != nil {
		t.Fatalf("failed to decode message: %v", err)
	}

	if msg.ID != "burst-msg-250" || msg.FromUID != "sender-uid" || msg.FromName != "Sender Name" || msg.RoomID != roomID {
		t.Errorf("message fields mismatch: %+v", msg)
	}

	// Cleanup
	t.Log("Cleaning up messages from Firestore...")
	docs, err = messagesColl.Documents(ctx).GetAll()
	if err == nil && len(docs) > 0 {
		batch := fsClient.Batch()
		for _, doc := range docs {
			batch.Delete(doc.Ref)
		}
		_, _ = batch.Commit(ctx)
	}
}

func TestPersistence_QueueOverflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize Hub
	chatHub := NewHub()
	go chatHub.RunWithContext(ctx)

	// Create worker but DO NOT start it, so its channel is never read
	// Use small queue buffer to ensure overflow
	worker := persistence.NewWorker(nil, 5)
	chatHub.SetPersistenceChan(worker.Queue())

	numMessages := 50

	t.Logf("Sending %d messages through Hub with a blocked/unstarted persistence worker queue...", numMessages)
	startTime := time.Now()

	// Send messages through Hub.
	// Since the worker is blocked, the persistence channel will fill up after 5 messages.
	// The Hub should drop the remaining 45 messages immediately without blocking.
	for i := 0; i < numMessages; i++ {
		env := &types.Envelope{
			ID:       fmt.Sprintf("overflow-msg-%d", i),
			Type:     types.TypeText,
			RoomID:   "overflow-room",
			FromUID:  "sender-uid",
			FromName: "Sender Name",
			Payload:  fmt.Sprintf("overflow payload %d", i),
		}
		chatHub.broadcast <- BroadcastMsg{Envelope: env}
	}

	duration := time.Since(startTime)
	t.Logf("Broadcast loop finished in %v", duration)

	if duration > 100*time.Millisecond {
		t.Errorf("Hub blocked on full persistence queue! Took %v, expected <100ms", duration)
	}
}
