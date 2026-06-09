package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"

	"cloud.google.com/go/firestore"
	"github.com/Dubjay/sanctum/internal/firebase"
)

func main() {
	credentialsPath := "./secrets/credentials.json"
	log.Printf("Initializing Firebase client with %s...", credentialsPath)
	fsClient, _, err := firebase.InitFirebase(credentialsPath)
	if err != nil {
		log.Fatalf("Failed to initialize Firebase: %v", err)
	}
	defer fsClient.Close()

	ctx := context.Background()
	docID := "test-msg-1"
	docRef := fsClient.Collection("rooms").Doc("test-room").Collection("messages").Doc(docID)

	testMsg := map[string]interface{}{
		"id":         docID,
		"from_uid":   "test-user-1",
		"from_name":  "Test User",
		"timestamp":  firestore.ServerTimestamp,
		"room_id":    "test-room",
		"ciphertext": "some-encrypted-payload-base64",
		"nonce":      "some-nonce-base64",
	}

	log.Printf("Writing document to /rooms/test-room/messages/%s...", docID)
	_, err = docRef.Set(ctx, testMsg)
	if err != nil {
		log.Fatalf("Failed to write message: %v", err)
	}

	fmt.Println("\n=====================================================================")
	fmt.Println("SUCCESS: Document written successfully.")
	fmt.Println("Please verify it appears in your Firestore Console:")
	fmt.Printf("Path: /rooms/test-room/messages/%s\n", docID)
	fmt.Println("=====================================================================")
	fmt.Print("\nPress [Enter] to delete the document and finish verification... ")

	bufio.NewReader(os.Stdin).ReadBytes('\n')

	log.Printf("Deleting document /rooms/test-room/messages/%s...", docID)
	_, err = docRef.Delete(ctx)
	if err != nil {
		log.Fatalf("Failed to delete message: %v", err)
	}

	fmt.Println("SUCCESS: Document deleted. Check Firestore Console to confirm deletion.")
}
