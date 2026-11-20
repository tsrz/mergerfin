package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
)

type mockDeltaSyncer struct {
	mu           sync.Mutex
	syncServerID []string
	triggerCalls int
	ch           chan string
}

func (m *mockDeltaSyncer) TriggerSync(ctx context.Context) error {
	m.mu.Lock()
	m.triggerCalls++
	m.mu.Unlock()
	return nil
}

func (m *mockDeltaSyncer) SyncServer(ctx context.Context, srv db.UpstreamServer) error {
	m.mu.Lock()
	m.syncServerID = append(m.syncServerID, srv.ID)
	m.mu.Unlock()
	if m.ch != nil {
		select {
		case m.ch <- srv.ID:
		default:
		}
	}
	return nil
}

func (m *mockDeltaSyncer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.syncServerID)
}

type mockBroadcaster struct {
	mu   sync.Mutex
	msgs []wsMessage
	ch   chan wsMessage
}

func (b *mockBroadcaster) Broadcast(messageType string, data any) {
	msg := wsMessage{MessageType: messageType, Data: data}
	b.mu.Lock()
	b.msgs = append(b.msgs, msg)
	b.mu.Unlock()
	if b.ch != nil {
		select {
		case b.ch <- msg:
		default:
		}
	}
}

func TestCheckServerHealthOnlineAndOffline(t *testing.T) {
	database := openTestDB(t)
	pool, err := NewPool(database, "health-secret", nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ctx := context.Background()

	var failMode atomic.Bool
	srvHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		if failMode.Load() {
			http.Error(w, "upstream overloaded", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ServerName": "Healthy JF",
			"Version":    "10.10.3",
		})
	}))
	defer srvHTTP.Close()

	srvID := idgen.NewRandomHex32()
	if err := database.UpsertServer(ctx, db.UpstreamServer{
		ID: srvID, Name: "JF1", BaseURL: srvHTTP.URL, Username: "u", Priority: 10, Enabled: true,
		ServerVersion: "10.9.0", IsOnline: false,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}

	srv, err := database.GetServerByID(ctx, srvID)
	if err != nil {
		t.Fatalf("GetServerByID: %v", err)
	}

	// Online check
	pool.CheckServerHealth(ctx, *srv)
	afterOnline, err := database.GetServerByID(ctx, srvID)
	if err != nil {
		t.Fatalf("GetServerByID after online: %v", err)
	}
	if !afterOnline.IsOnline || afterOnline.ServerVersion != "10.10.3" || afterOnline.LastError != "" || afterOnline.LatencyMs < 0 {
		t.Fatalf("unexpected online status: %+v", afterOnline)
	}

	// Offline check (HTTP 503) preserves ServerVersion and records LastError
	failMode.Store(true)
	pool.CheckServerHealth(ctx, *afterOnline)
	afterOffline, err := database.GetServerByID(ctx, srvID)
	if err != nil {
		t.Fatalf("GetServerByID after offline: %v", err)
	}
	if afterOffline.IsOnline || afterOffline.ServerVersion != "10.10.3" || afterOffline.LastError == "" {
		t.Fatalf("unexpected offline status: %+v", afterOffline)
	}
}

func TestListenerManagerHandshakeKeepAliveAndDebouncedSync(t *testing.T) {
	database := openTestDB(t)
	pool, err := NewPool(database, "listener-secret", nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ctx := context.Background()

	const upstreamToken = "up-ws-token-999"
	srvID := idgen.NewRandomHex32()

	keepAliveAcked := make(chan struct{}, 1)
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("ApiKey") != upstreamToken || r.URL.Query().Get("api_key") != upstreamToken {
			http.Error(w, "missing ApiKey/api_key", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("deviceId") != "mergerfin-listener-"+srvID {
			http.Error(w, "unexpected deviceId", http.StatusBadRequest)
			return
		}
		authHdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHdr, "MediaBrowser ") || !strings.Contains(authHdr, `Token="`+upstreamToken+`"`) {
			http.Error(w, "missing MediaBrowser auth header", http.StatusUnauthorized)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()

		// 1. Send ForceKeepAlive and wait for KeepAlive reply
		_ = wsjson.Write(r.Context(), conn, wsMessage{MessageType: "ForceKeepAlive", Data: 60})
		var reply wsMessage
		if err := wsjson.Read(r.Context(), conn, &reply); err != nil {
			return
		}
		if strings.EqualFold(reply.MessageType, "KeepAlive") {
			select {
			case keepAliveAcked <- struct{}{}:
			default:
			}
		}

		// 2. Send two rapid LibraryChanged notifications (should debounce into 1 SyncServer call)
		_ = wsjson.Write(r.Context(), conn, wsMessage{MessageType: "LibraryChanged", Data: map[string]any{"ItemsAdded": []string{"a"}}})
		_ = wsjson.Write(r.Context(), conn, wsMessage{MessageType: "LibraryChanged", Data: map[string]any{"ItemsAdded": []string{"b"}}})

		// Keep connection open until client closes on ctx cancel
		for {
			var sink wsMessage
			if err := wsjson.Read(r.Context(), conn, &sink); err != nil {
				return
			}
		}
	}))
	defer wsSrv.Close()

	encTok, err := pool.EncryptToken(upstreamToken)
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	if err := database.UpsertServer(ctx, db.UpstreamServer{
		ID: srvID, Name: "WSUp", BaseURL: wsSrv.URL, Username: "u", RemoteUserID: "ru",
		AccessToken: encTok, Priority: 10, StreamMode: "proxy", Enabled: true, IsOnline: true,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}

	syncer := &mockDeltaSyncer{ch: make(chan string, 4)}
	broadcaster := &mockBroadcaster{ch: make(chan wsMessage, 4)}

	mgr := NewListenerManager(pool, syncer, broadcaster, nil)
	mgr.ReconcileInterval = 200 * time.Millisecond
	mgr.DebounceDelay = 80 * time.Millisecond
	mgr.BackoffSchedule = []time.Duration{50 * time.Millisecond}

	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		mgr.Run(runCtx)
	}()

	select {
	case <-keepAliveAcked:
	case <-time.After(3 * time.Second):
		cancel()
		<-runDone
		t.Fatalf("timed out waiting for KeepAlive reply to ForceKeepAlive")
	}

	select {
	case gotID := <-syncer.ch:
		if gotID != srvID {
			cancel()
			<-runDone
			t.Fatalf("unexpected synced server ID: got %q, want %q", gotID, srvID)
		}
	case <-time.After(3 * time.Second):
		cancel()
		<-runDone
		t.Fatalf("timed out waiting for debounced SyncServer call")
	}

	// Allow a brief window to ensure both rapid LibraryChanged messages collapsed into 1 SyncServer call
	time.Sleep(150 * time.Millisecond)
	if syncer.count() != 1 {
		cancel()
		<-runDone
		t.Fatalf("expected 1 debounced SyncServer call, got %d", syncer.count())
	}

	select {
	case bmsg := <-broadcaster.ch:
		if bmsg.MessageType != "LibraryChanged" {
			cancel()
			<-runDone
			t.Fatalf("unexpected broadcast message type: %q", bmsg.MessageType)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-runDone
		t.Fatalf("timed out waiting for downstream broadcast")
	}

	// Verify clean, race-free shutdown on context cancel
	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("ListenerManager.Run did not exit cleanly after context cancel")
	}
}
