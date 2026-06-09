package firebase

import (
	"context"
	"testing"

	"cloud.google.com/go/firestore"
)

func TestFirestore_WriteReadDelete(t *testing.T) {
	credentialsPath := "../../secrets/credentials.json"
	fsClient, _, err := InitFirebase(credentialsPath)
	if err != nil {
		t.Fatalf("failed to initialize firebase: %v", err)
	}
	defer fsClient.Close()

	ctx := context.Background()
	docRef := fsClient.Collection("rooms").Doc("test-room").Collection("messages").Doc("test-msg-1")

	// 1. Write the test message document
	testMsg := map[string]interface{}{
		"id":         "test-msg-1",
		"from_uid":   "test-user-1",
		"from_name":  "Test User",
		"timestamp":  firestore.ServerTimestamp,
		"room_id":    "test-room",
		"ciphertext": "some-encrypted-payload-base64",
		"nonce":      "some-nonce-base64",
	}

	_, err = docRef.Set(ctx, testMsg)
	if err != nil {
		t.Fatalf("failed to write test message: %v", err)
	}
	t.Logf("Successfully wrote test-msg-1 to Firestore.")

	// 2. Read it back
	snapshot, err := docRef.Get(ctx)
	if err != nil {
		t.Fatalf("failed to read test message: %v", err)
	}

	data := snapshot.Data()
	if data["id"] != "test-msg-1" {
		t.Errorf("expected id 'test-msg-1', got '%v'", data["id"])
	}
	if data["from_uid"] != "test-user-1" {
		t.Errorf("expected from_uid 'test-user-1', got '%v'", data["from_uid"])
	}
	if data["from_name"] != "Test User" {
		t.Errorf("expected from_name 'Test User', got '%v'", data["from_name"])
	}
	if data["room_id"] != "test-room" {
		t.Errorf("expected room_id 'test-room', got '%v'", data["room_id"])
	}

	// 3. Delete it
	_, err = docRef.Delete(ctx)
	if err != nil {
		t.Fatalf("failed to delete test message: %v", err)
	}
	t.Logf("Successfully deleted test-msg-1 from Firestore.")

	// 4. Confirm deletion
	_, err = docRef.Get(ctx)
	if err == nil {
		t.Errorf("expected error reading deleted document, got nil")
	}
}
