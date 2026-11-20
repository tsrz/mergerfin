package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsrz/mergerfin/internal/db"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), "MergerFin-Test")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func newMockJellyfin(t *testing.T, serverName, version, expectedUser, expectedPass, remoteUserID, accessToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ServerName": serverName,
				"Version":    version,
				"Id":         "abcdef0123456789abcdef0123456789",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			authHdr := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHdr, "MediaBrowser ") || !strings.Contains(authHdr, `Client="MergerFin"`) {
				http.Error(w, "missing MediaBrowser header", http.StatusBadRequest)
				return
			}
			var body struct {
				Username string `json:"Username"`
				Pw       string `json:"Pw"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			if body.Username != expectedUser || body.Pw != expectedPass {
				http.Error(w, "invalid credentials", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"User": map[string]any{
					"Id":   remoteUserID,
					"Name": expectedUser,
				},
				"AccessToken": accessToken,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	database := openTestDB(t)

	pool, err := NewPool(database, "super-secret-passphrase", nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	plain := "jf_token_abc123_xyz789"
	enc1, err := pool.EncryptToken(plain)
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	if !strings.HasPrefix(enc1, "enc:") {
		t.Fatalf("expected enc: prefix, got %q", enc1)
	}
	if enc1 == plain || strings.Contains(enc1, plain) {
		t.Fatalf("encrypted token leaked plaintext: %q", enc1)
	}

	enc2, err := pool.EncryptToken(plain)
	if err != nil {
		t.Fatalf("EncryptToken second call: %v", err)
	}
	if enc1 == enc2 {
		t.Fatalf("expected unique random nonces on each encryption")
	}

	dec, err := pool.DecryptToken(enc1)
	if err != nil {
		t.Fatalf("DecryptToken: %v", err)
	}
	if dec != plain {
		t.Fatalf("DecryptToken mismatch: got %q, want %q", dec, plain)
	}

	// Unencrypted stored values pass through unchanged.
	unenc, err := pool.DecryptToken("raw-legacy-token")
	if err != nil || unenc != "raw-legacy-token" {
		t.Fatalf("DecryptToken raw passthrough: got (%q, %v)", unenc, err)
	}

	// Different secret key fails decryption.
	otherPool, err := NewPool(database, "different-secret-key", nil)
	if err != nil {
		t.Fatalf("NewPool other: %v", err)
	}
	if _, err := otherPool.DecryptToken(enc1); err == nil {
		t.Fatalf("expected decryption error with wrong secret key")
	}
}

func TestEncryptDecryptNoSecretKey(t *testing.T) {
	database := openTestDB(t)
	pool, err := NewPool(database, "", nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	enc, err := pool.EncryptToken("plain-tok")
	if err != nil || enc != "plain-tok" {
		t.Fatalf("expected passthrough without secretKey, got (%q, %v)", enc, err)
	}
	dec, err := pool.DecryptToken("plain-tok")
	if err != nil || dec != "plain-tok" {
		t.Fatalf("expected passthrough decrypt, got (%q, %v)", dec, err)
	}
	if _, err := pool.DecryptToken("enc:AAAA"); err == nil {
		t.Fatalf("expected error decrypting enc: token when no secretKey configured")
	}
}

func TestTestConnection(t *testing.T) {
	database := openTestDB(t)
	pool, err := NewPool(database, "secret", nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	jf := newMockJellyfin(t, "Living Room JF", "10.9.11", "alice", "pass123", "remote-user-1", "tok-secret-1")

	res, err := pool.TestConnection(context.Background(), jf.URL+"/", "alice", "pass123")
	if err != nil {
		t.Fatalf("TestConnection success case failed: %v", err)
	}
	if !res.Online || res.ServerName != "Living Room JF" || res.Version != "10.9.11" {
		t.Fatalf("unexpected ProbeResult: %+v", res)
	}
	if res.RemoteUserID != "remote-user-1" || res.AccessToken != "tok-secret-1" {
		t.Fatalf("unexpected auth fields: %+v", res)
	}

	// Bad credentials
	badRes, err := pool.TestConnection(context.Background(), jf.URL, "alice", "wrongpass")
	if err == nil {
		t.Fatalf("expected error on wrong password")
	}
	if badRes.AccessToken != "" || badRes.Error == "" {
		t.Fatalf("expected empty AccessToken and error message, got %+v", badRes)
	}

	// Unreachable URL
	offRes, err := pool.TestConnection(context.Background(), "http://127.0.0.1:1", "alice", "pass123")
	if err == nil || offRes.Online {
		t.Fatalf("expected offline failure, got (%+v, %v)", offRes, err)
	}
}

func TestServerPoolCRUDAndDecryption(t *testing.T) {
	database := openTestDB(t)
	pool, err := NewPool(database, "pool-encryption-key", nil)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ctx := context.Background()

	jf1 := newMockJellyfin(t, "Server One", "10.9.11", "user1", "pw1", "ru1", "plain-token-1")
	jf2 := newMockJellyfin(t, "Server Two", "10.10.0", "user2", "pw2", "ru2", "plain-token-2")

	s1, err := pool.AddOrUpdateServer(ctx, "", "Alpha", jf1.URL, "user1", "pw1", 10, "proxy", true)
	if err != nil {
		t.Fatalf("AddOrUpdateServer s1: %v", err)
	}
	if !strings.HasPrefix(s1.AccessToken, "enc:") {
		t.Fatalf("expected stored token to be encrypted, got %q", s1.AccessToken)
	}

	s2, err := pool.AddOrUpdateServer(ctx, "", "Beta", jf2.URL, "user2", "pw2", 20, "redirect", false)
	if err != nil {
		t.Fatalf("AddOrUpdateServer s2: %v", err)
	}

	// ListServers returns encrypted tokens.
	all, err := pool.ListServers(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListServers: len=%d err=%v", len(all), err)
	}

	// GetDecryptedServer returns plaintext token.
	dec1, err := pool.GetDecryptedServer(ctx, s1.ID)
	if err != nil {
		t.Fatalf("GetDecryptedServer: %v", err)
	}
	if dec1.AccessToken != "plain-token-1" || dec1.RemoteUserID != "ru1" {
		t.Fatalf("unexpected decrypted server: %+v", dec1)
	}

	// ListEnabledDecryptedServers only returns enabled servers.
	enabled, err := pool.ListEnabledDecryptedServers(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].ID != s1.ID || enabled[0].AccessToken != "plain-token-1" {
		t.Fatalf("unexpected enabled servers: %+v, err=%v", enabled, err)
	}

	// Update s2 without password keeps existing encrypted token and enables it.
	_, err = pool.AddOrUpdateServer(ctx, s2.ID, "Beta Updated", jf2.URL, "user2", "", 5, "redirect", true)
	if err != nil {
		t.Fatalf("AddOrUpdateServer update without pw: %v", err)
	}
	enabled, err = pool.ListEnabledDecryptedServers(ctx)
	if err != nil || len(enabled) != 2 {
		t.Fatalf("expected 2 enabled servers, got %d (err=%v)", len(enabled), err)
	}
	// Priority 5 comes before priority 10.
	if enabled[0].ID != s2.ID || enabled[0].AccessToken != "plain-token-2" || enabled[0].Name != "Beta Updated" {
		t.Fatalf("unexpected first enabled server after update: %+v", enabled[0])
	}

	if err := pool.DeleteServer(ctx, s1.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	all, err = pool.ListServers(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("expected 1 server after delete, got %d (err=%v)", len(all), err)
	}
}
