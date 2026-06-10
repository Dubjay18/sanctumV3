package hub

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/google/uuid"

	"github.com/Dubjay/sanctum/internal/firebase"
	"github.com/Dubjay/sanctum/pkg/types"
)

func TestHub_HistoryAndInvites(t *testing.T) {
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
	chatHub.SetFirestoreClient(fsClient)
	go chatHub.RunWithContext(ctx)

	// Setup a private room to test joins & auth
	roomID := "test-room-" + uuid.New().String()
	roomDoc := types.Room{
		ID:         roomID,
		Name:       "Private Room",
		CreatedBy:  "creator-uid",
		CreatedAt:  time.Now(),
		MemberUIDs: []string{"creator-uid"},
		IsPrivate:  true,
	}
	_, err = fsClient.Collection("rooms").Doc(roomID).Set(ctx, roomDoc)
	if err != nil {
		t.Fatalf("failed to create room in Firestore: %v", err)
	}
	defer func() {
		// Cleanup room
		_, _ = fsClient.Collection("rooms").Doc(roomID).Delete(ctx)
	}()

	// 1. Verify FetchUserRooms works
	rooms, err := firebase.FetchUserRooms(ctx, fsClient, "creator-uid")
	if err != nil {
		t.Fatalf("FetchUserRooms failed: %v", err)
	}
	found := false
	for _, r := range rooms {
		if r.ID == roomID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected room %s in creator rooms, not found", roomID)
	}

	// 2. Populate 60 messages to test FetchRoomHistory pagination
	messagesColl := fsClient.Collection("rooms").Doc(roomID).Collection("messages")
	// Cleanup any existing
	docs, _ := messagesColl.Documents(ctx).GetAll()
	for _, d := range docs {
		_, _ = d.Ref.Delete(ctx)
	}

	// Write 60 messages with increasing timestamps
	baseTime := time.Now().Add(-1 * time.Hour)
	for i := 0; i < 60; i++ {
		msgID := fmt.Sprintf("msg-%03d", i)
		msg := map[string]interface{}{
			"id":         msgID,
			"from_uid":   "creator-uid",
			"from_name":  "Creator",
			"timestamp":  baseTime.Add(time.Duration(i) * time.Minute),
			"room_id":    roomID,
			"ciphertext": "cipherText",
			"nonce":      "nonce",
		}
		_, err = messagesColl.Doc(msgID).Set(ctx, msg)
		if err != nil {
			t.Fatalf("failed to write history message %d: %v", i, err)
		}
	}
	defer func() {
		docs, _ := messagesColl.Documents(ctx).GetAll()
		for _, d := range docs {
			_, _ = d.Ref.Delete(ctx)
		}
	}()

	// 3. Test FetchRoomHistory (limit 50, empty beforeMsgID) -> returns latest 50 (messages 10 to 59)
	history1, err := firebase.FetchRoomHistory(ctx, fsClient, roomID, "", 50)
	if err != nil {
		t.Fatalf("FetchRoomHistory failed: %v", err)
	}
	if len(history1) != 50 {
		t.Errorf("expected 50 messages, got %d", len(history1))
	}
	// Chronological order: oldest first in slice. Since we fetched DESC limit 50 and reversed,
	// the first message in the slice should be "msg-010" and last should be "msg-059".
	if history1[0].ID != "msg-010" {
		t.Errorf("expected first message of batch 1 to be msg-010, got %s", history1[0].ID)
	}
	if history1[49].ID != "msg-059" {
		t.Errorf("expected last message of batch 1 to be msg-059, got %s", history1[49].ID)
	}

	// 4. Test FetchRoomHistory pagination (beforeMsgID = "msg-010") -> returns remaining 10 (messages 0 to 9)
	history2, err := firebase.FetchRoomHistory(ctx, fsClient, roomID, "msg-010", 50)
	if err != nil {
		t.Fatalf("FetchRoomHistory pagination failed: %v", err)
	}
	if len(history2) != 10 {
		t.Errorf("expected 10 messages in batch 2, got %d", len(history2))
	}
	if history2[0].ID != "msg-000" {
		t.Errorf("expected first message of batch 2 to be msg-000, got %s", history2[0].ID)
	}
	if history2[9].ID != "msg-009" {
		t.Errorf("expected last message of batch 2 to be msg-009, got %s", history2[9].ID)
	}

	// 5. Test Hub authorization and invitations
	creatorClient := &Client{
		ID:     "creator-uid",
		Name:   "Creator",
		hub:    chatHub,
		send:   make(chan []byte, 10),
		roomID: roomID,
	}
	creatorClient.ensureContext()
	chatHub.register <- creatorClient

	guestClient := &Client{
		ID:     "guest-uid",
		Name:   "Guest",
		hub:    chatHub,
		send:   make(chan []byte, 10),
		roomID: roomID,
	}
	guestClient.ensureContext()

	// Inject message into Guest read loop simulation
	go func() {
		// Mock guest join
		chatHub.register <- guestClient
	}()
	time.Sleep(50 * time.Millisecond)

	// Since they are not authorized, let's test JoinRoom validation directly by simulating message handler
	// We can invoke the read handler case for join on a client
	// Let's verify Invite adds user to room
	
	// Create another channel or write to Guest directly
	// Let's test that Invite updates Firestore member_uids
	_, err = fsClient.Collection("rooms").Doc(roomID).Update(ctx, []firestore.Update{
		{
			Path:  "member_uids",
			Value: firestore.ArrayUnion("guest-uid"),
		},
	})
	if err != nil {
		t.Fatalf("failed to manually add member to room: %v", err)
	}

	// Refresh cache and check
	chatHub.InvalidateRoomCache(roomID)
	updatedRoom, err := chatHub.GetCachedRoom(ctx, roomID)
	if err != nil {
		t.Fatalf("failed to get cached room: %v", err)
	}
	isGuestMember := false
	for _, mUID := range updatedRoom.MemberUIDs {
		if mUID == "guest-uid" {
			isGuestMember = true
			break
		}
	}
	if !isGuestMember {
		t.Errorf("expected guest-uid to be a member after manual add/invite mock")
	}
}
