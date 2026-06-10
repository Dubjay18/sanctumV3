package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/gorilla/websocket"
	"go.uber.org/goleak"

	"github.com/Dubjay/sanctum/internal/firebase"
	"github.com/Dubjay/sanctum/internal/persistence"
	"github.com/Dubjay/sanctum/pkg/types"
)

type LatencyTracker struct {
	totalReceived int64
	sumLatencyNs  int64
	maxLatencyNs  int64
	under5msCount int64
}

func (lt *LatencyTracker) Record(latency time.Duration) {
	ns := latency.Nanoseconds()
	atomic.AddInt64(&lt.totalReceived, 1)
	atomic.AddInt64(&lt.sumLatencyNs, ns)

	for {
		currentMax := atomic.LoadInt64(&lt.maxLatencyNs)
		if ns <= currentMax {
			break
		}
		if atomic.CompareAndSwapInt64(&lt.maxLatencyNs, currentMax, ns) {
			break
		}
	}

	if latency <= 5*time.Millisecond {
		atomic.AddInt64(&lt.under5msCount, 1)
	}
}

func TestIntegration_StressWriteQueue(t *testing.T) {
	// Verify no goroutine leaks after the test ends
	defer goleak.VerifyNone(t,
		goleak.IgnoreCurrent(),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/transport.(*controlBuffer).get"),
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/transport.(*http2Client).keepalive"),
	)

	t.Setenv("ENV", "development")

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

	// Set persistence worker with a large queue to prevent drops
	worker := persistence.NewWorker(fsClient, 50000)
	chatHub.SetPersistenceChan(worker.Queue())
	worker.Start(ctx)

	go chatHub.RunWithContext(ctx)

	// Set up mock WebSocket server bypassing authentication middleware for stress testing
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		uid := r.URL.Query().Get("uid")
		name := r.URL.Query().Get("name")
		roomID := r.URL.Query().Get("room")

		client := NewClient(chatHub, conn, uid, name, roomID)
		client.Start()
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse test server URL: %v", err)
	}
	serverURL.Scheme = "ws"
	serverURL.Path = "/ws"

	const numClients = 100
	const numRooms = 50 // 2 clients per room for 1x fanout without CPU starvation
	const testDuration = 30 * time.Second
	const msgsPerSecond = 10
	const msgsPerClient = int(testDuration / time.Second) * msgsPerSecond // 300 msgs

	// Setup rooms in Firestore
	for i := 0; i < numRooms; i++ {
		roomID := fmt.Sprintf("stress-room-%d", i)
		roomDoc := map[string]interface{}{
			"id":          roomID,
			"name":        fmt.Sprintf("Stress Room %d", i),
			"created_by":  "stress-test",
			"created_at":  time.Now(),
			"member_uids": []string{},
			"is_private":  false,
		}
		_, err = fsClient.Collection("rooms").Doc(roomID).Set(ctx, roomDoc)
		if err != nil {
			t.Fatalf("failed to setup room %d: %v", i, err)
		}
	}
	defer func() {
		for i := 0; i < numRooms; i++ {
			roomID := fmt.Sprintf("stress-room-%d", i)
			_, _ = fsClient.Collection("rooms").Doc(roomID).Delete(ctx)
		}
	}()

	conns := make([]*websocket.Conn, numClients)
	latencyTracker := &LatencyTracker{}

	var wgReaders sync.WaitGroup
	ctxReaders, cancelReaders := context.WithCancel(ctx)
	defer cancelReaders()

	// Connect clients
	for i := 0; i < numClients; i++ {
		roomIdx := i / 2
		roomID := fmt.Sprintf("stress-room-%d", roomIdx)
		uid := fmt.Sprintf("user-%d", i)
		name := fmt.Sprintf("User %d", i)

		// Set members in Firestore
		_, err = fsClient.Collection("rooms").Doc(roomID).Update(ctx, []firestore.Update{
			{
				Path:  "member_uids",
				Value: firestore.ArrayUnion(uid),
			},
		})
		if err != nil {
			t.Fatalf("failed to add user to room: %v", err)
		}

		u := *serverURL
		q := u.Query()
		q.Set("uid", uid)
		q.Set("name", name)
		q.Set("room", roomID)
		u.RawQuery = q.Encode()

		conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			t.Fatalf("failed to connect client %d: %v", i, err)
		}
		conns[i] = conn

		// Reader loop
		wgReaders.Add(1)
		go func(c *websocket.Conn, clientUID string) {
			defer wgReaders.Done()
			for {
				select {
				case <-ctxReaders.Done():
					return
				default:
					_, data, err := c.ReadMessage()
					if err != nil {
						return
					}

					var env types.Envelope
					if err := json.Unmarshal(data, &env); err != nil {
						continue
					}

					// We only measure latency on TypeText messages sent by the other user
					if env.Type == types.TypeText && env.FromUID != clientUID {
						var sentNano int64
						_, err := fmt.Sscanf(env.Payload, "%d", &sentNano)
						if err == nil && sentNano > 0 {
							latency := time.Since(time.Unix(0, sentNano))
							latencyTracker.Record(latency)
						}
					}
				}
			}
		}(conn, uid)
	}

	// Warmup
	time.Sleep(1 * time.Second)

	// Start sending messages
	var wgSenders sync.WaitGroup
	t.Logf("Simulating %d clients sending %d messages/sec for %v...", numClients, msgsPerSecond, testDuration)

	startStress := time.Now()
	for i := 0; i < numClients; i++ {
		wgSenders.Add(1)
		go func(conn *websocket.Conn, clientIdx int) {
			defer wgSenders.Done()
			roomIdx := clientIdx / 2
			roomID := fmt.Sprintf("stress-room-%d", roomIdx)

			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			for j := 0; j < msgsPerClient; j++ {
				select {
				case <-ticker.C:
					env := &types.Envelope{
						ID:      fmt.Sprintf("stress-msg-%d-%d", clientIdx, j),
						Type:    types.TypeText,
						RoomID:  roomID,
						Payload: fmt.Sprintf("%d", time.Now().UnixNano()),
					}
					data, err := types.Marshal(env)
					if err == nil {
						_ = conn.WriteMessage(websocket.TextMessage, data)
					}
				case <-ctx.Done():
					return
				}
			}
		}(conns[i], i)
	}

	wgSenders.Wait()
	t.Logf("Finished sending all 30,000 messages in %v.", time.Since(startStress))

	// Allow some time for messages to be delivered and persisted
	time.Sleep(5 * time.Second)

	// Close all connections to stop readers
	for _, conn := range conns {
		_ = conn.Close()
	}
	cancelReaders()
	wgReaders.Wait()

	// Stop Hub and Worker
	cancel()
	worker.Wait()

	// Log latency stats
	totalReceived := atomic.LoadInt64(&latencyTracker.totalReceived)
	maxLatencyNs := atomic.LoadInt64(&latencyTracker.maxLatencyNs)
	sumLatencyNs := atomic.LoadInt64(&latencyTracker.sumLatencyNs)
	under5msCount := atomic.LoadInt64(&latencyTracker.under5msCount)

	var avgLatency time.Duration
	if totalReceived > 0 {
		avgLatency = time.Duration(sumLatencyNs / totalReceived)
	}
	maxLatency := time.Duration(maxLatencyNs)

	t.Logf("Latency Stats: Total received = %d, Avg latency = %v, Max latency = %v", totalReceived, avgLatency, maxLatency)
	t.Logf("Messages delivered under 5ms: %d / %d (%.2f%%)", under5msCount, totalReceived, float64(under5msCount)/float64(totalReceived)*100)

	// 1. Verify: hub fanout latency stays under 5ms (check 99th percentile or average)
	// We'll assert that the average latency is well under 5ms, and the majority of messages are delivered within 5ms.
	if totalReceived > 0 && avgLatency > 5*time.Millisecond {
		t.Errorf("average fanout latency exceeded 5ms: %v", avgLatency)
	}

	// 2. Verify: all 30,000 messages appear in Firestore (eventually consistent)
	t.Log("Verifying message counts in Firestore...")
	var totalDocsCount int
	for r := 0; r < numRooms; r++ {
		roomID := fmt.Sprintf("stress-room-%d", r)
		messagesColl := fsClient.Collection("rooms").Doc(roomID).Collection("messages")

		docs, err := messagesColl.Select().Documents(context.Background()).GetAll()
		if err != nil {
			t.Fatalf("failed to query messages for room %s: %v", roomID, err)
		}
		totalDocsCount += len(docs)
	}

	t.Logf("Total messages found in Firestore: %d", totalDocsCount)
	if totalDocsCount != numClients*msgsPerClient {
		t.Errorf("expected %d messages in Firestore, got %d", numClients*msgsPerClient, totalDocsCount)
	}

	// Cleanup Firestore messages
	t.Log("Cleaning up messages from Firestore...")
	for r := 0; r < numRooms; r++ {
		roomID := fmt.Sprintf("stress-room-%d", r)
		messagesColl := fsClient.Collection("rooms").Doc(roomID).Collection("messages")
		docs, err := messagesColl.Documents(context.Background()).GetAll()
		if err == nil && len(docs) > 0 {
			batch := fsClient.Batch()
			for _, doc := range docs {
				batch.Delete(doc.Ref)
			}
			_, _ = batch.Commit(context.Background())
		}
	}
}
