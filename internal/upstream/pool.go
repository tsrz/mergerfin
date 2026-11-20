package upstream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"golang.org/x/crypto/pbkdf2"
)

const (
	encPrefix           = "enc:"
	pbkdf2Iterations    = 100000
	aes256KeyBytes      = 32
	jellyfinAuthHeader  = `MediaBrowser Client="MergerFin", Device="MergerFin Proxy", DeviceId="mergerfin-proxy", Version="12.4.0"`
	defaultServerPrio   = 100
	defaultStreamMode   = "proxy"
	redirectStreamMode  = "redirect"
	probeHTTPTimeout    = 10 * time.Second
	maxProbeBodyBytes   = 1 << 20
)

// ProbeResult represents the outcome of probing and authenticating against an upstream Jellyfin server.
type ProbeResult struct {
	Online       bool   `json:"online"`
	LatencyMs    int64  `json:"latencyMs"`
	ServerName   string `json:"serverName"`
	Version      string `json:"version"`
	RemoteUserID string `json:"remoteUserId,omitempty"`
	AccessToken  string `json:"-"`
	Error        string `json:"error,omitempty"`
}

// Pool manages upstream Jellyfin servers, token encryption, and connectivity probing.
type Pool struct {
	db         *db.DB
	key        []byte
	logger     *slog.Logger
	httpClient *http.Client
}

// NewPool creates an upstream server pool. When secretKey is non-empty, it derives an
// AES-256 key from secretKey and the persisted encryption_salt setting via PBKDF2-SHA256.
func NewPool(database *db.DB, secretKey string, logger *slog.Logger) (*Pool, error) {
	if database == nil {
		return nil, errors.New("database must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	var derivedKey []byte
	if secretKey != "" {
		salt, err := database.GetSetting(context.Background(), "encryption_salt")
		if err != nil {
			return nil, fmt.Errorf("load encryption_salt setting: %w", err)
		}
		if salt == "" {
			return nil, errors.New("encryption_salt setting is empty")
		}
		derivedKey = pbkdf2.Key([]byte(secretKey), []byte(salt), pbkdf2Iterations, aes256KeyBytes, sha256.New)
	}

	return &Pool{
		db:     database,
		key:    derivedKey,
		logger: logger,
		httpClient: &http.Client{
			Timeout: probeHTTPTimeout,
		},
	}, nil
}

// EncryptToken encrypts a plaintext token with AES-256-GCM and prefixes "enc:" + base64,
// or returns plain unchanged when no secretKey was configured.
func (p *Pool) EncryptToken(plain string) (string, error) {
	if len(p.key) == 0 {
		return plain, nil
	}

	block, err := aes.NewCipher(p.key)
	if err != nil {
		return "", fmt.Errorf("create aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptToken decrypts an "enc:"-prefixed token or returns stored unchanged if unencrypted.
func (p *Pool) DecryptToken(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	if len(p.key) == 0 {
		return "", errors.New("cannot decrypt token: encryption key is not configured")
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted token: %w", err)
	}

	block, err := aes.NewCipher(p.key)
	if err != nil {
		return "", fmt.Errorf("create aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("encrypted token payload is too short")
	}

	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt token: %w", err)
	}
	return string(plain), nil
}

func normalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", errors.New("baseURL must not be empty")
	}
	u, err := url.ParseRequestURI(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid baseURL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid baseURL scheme %q: must be http or https", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid baseURL %q: missing host", raw)
	}
	return trimmed, nil
}

// TestConnection probes GET <baseURL>/System/Info/Public and authenticates via
// POST <baseURL>/Users/AuthenticateByName to verify reachability and credentials.
func (p *Pool) TestConnection(ctx context.Context, baseURL, username, password string) (ProbeResult, error) {
	cleanURL, err := normalizeBaseURL(baseURL)
	if err != nil {
		return ProbeResult{Online: false, Error: err.Error()}, err
	}

	// 1. Public info probe
	infoReq, err := http.NewRequestWithContext(ctx, http.MethodGet, cleanURL+"/System/Info/Public", nil)
	if err != nil {
		res := ProbeResult{Online: false, Error: err.Error()}
		return res, err
	}
	infoReq.Header.Set("Accept", "application/json")

	start := time.Now()
	infoResp, err := p.httpClient.Do(infoReq)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		res := ProbeResult{Online: false, LatencyMs: latencyMs, Error: err.Error()}
		return res, fmt.Errorf("probe /System/Info/Public: %w", err)
	}
	infoBody, readErr := io.ReadAll(io.LimitReader(infoResp.Body, maxProbeBodyBytes))
	_ = infoResp.Body.Close()
	if readErr != nil {
		res := ProbeResult{Online: false, LatencyMs: latencyMs, Error: readErr.Error()}
		return res, fmt.Errorf("read /System/Info/Public response: %w", readErr)
	}
	if infoResp.StatusCode != http.StatusOK {
		errMsg := fmt.Sprintf("upstream /System/Info/Public returned HTTP %d", infoResp.StatusCode)
		res := ProbeResult{Online: false, LatencyMs: latencyMs, Error: errMsg}
		return res, errors.New(errMsg)
	}

	var pubInfo struct {
		ServerName string `json:"ServerName"`
		Version    string `json:"Version"`
	}
	if err := json.Unmarshal(infoBody, &pubInfo); err != nil {
		errMsg := fmt.Sprintf("invalid /System/Info/Public JSON: %v", err)
		res := ProbeResult{Online: false, LatencyMs: latencyMs, Error: errMsg}
		return res, fmt.Errorf("decode /System/Info/Public: %w", err)
	}

	res := ProbeResult{
		Online:     true,
		LatencyMs:  latencyMs,
		ServerName: pubInfo.ServerName,
		Version:    pubInfo.Version,
	}

	// 2. AuthenticateByName
	payload, err := json.Marshal(struct {
		Username string `json:"Username"`
		Pw       string `json:"Pw"`
	}{
		Username: username,
		Pw:       password,
	})
	if err != nil {
		res.Error = err.Error()
		return res, err
	}

	authReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cleanURL+"/Users/AuthenticateByName", bytes.NewReader(payload))
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	authReq.Header.Set("Content-Type", "application/json")
	authReq.Header.Set("Accept", "application/json")
	authReq.Header.Set("Authorization", jellyfinAuthHeader)

	authResp, err := p.httpClient.Do(authReq)
	if err != nil {
		res.Error = err.Error()
		return res, fmt.Errorf("authenticate against upstream: %w", err)
	}
	authBody, readErr := io.ReadAll(io.LimitReader(authResp.Body, maxProbeBodyBytes))
	_ = authResp.Body.Close()
	if readErr != nil {
		res.Error = readErr.Error()
		return res, fmt.Errorf("read auth response: %w", readErr)
	}
	if authResp.StatusCode != http.StatusOK {
		errMsg := fmt.Sprintf("authentication failed (HTTP %d)", authResp.StatusCode)
		res.Error = errMsg
		return res, errors.New(errMsg)
	}

	var authResult struct {
		User struct {
			ID string `json:"Id"`
		} `json:"User"`
		AccessToken string `json:"AccessToken"`
	}
	if err := json.Unmarshal(authBody, &authResult); err != nil {
		res.Error = fmt.Sprintf("invalid auth JSON: %v", err)
		return res, fmt.Errorf("decode auth response: %w", err)
	}
	if authResult.AccessToken == "" {
		errMsg := "upstream authentication returned empty AccessToken"
		res.Error = errMsg
		return res, errors.New(errMsg)
	}

	res.RemoteUserID = authResult.User.ID
	res.AccessToken = authResult.AccessToken
	return res, nil
}

// AddOrUpdateServer creates a new upstream server (when id == "") or updates an existing one.
// Creating a server or updating with a non-empty password tests the connection and encrypts the returned token.
func (p *Pool) AddOrUpdateServer(ctx context.Context, id, name, baseURL, username, password string, priority int, streamMode string, enabled bool) (*db.UpstreamServer, error) {
	cleanName := strings.TrimSpace(name)
	if cleanName == "" {
		return nil, errors.New("server name must not be empty")
	}
	cleanURL, err := normalizeBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	cleanUser := strings.TrimSpace(username)
	if cleanUser == "" {
		return nil, errors.New("username must not be empty")
	}

	mode := strings.ToLower(strings.TrimSpace(streamMode))
	if mode == "" {
		mode = defaultStreamMode
	}
	if mode != defaultStreamMode && mode != redirectStreamMode {
		return nil, fmt.Errorf("invalid streamMode %q: must be %q or %q", streamMode, defaultStreamMode, redirectStreamMode)
	}
	if priority <= 0 {
		priority = defaultServerPrio
	}

	if id == "" {
		probe, err := p.TestConnection(ctx, cleanURL, cleanUser, password)
		if err != nil {
			return nil, err
		}
		encToken, err := p.EncryptToken(probe.AccessToken)
		if err != nil {
			return nil, fmt.Errorf("encrypt access token: %w", err)
		}

		srv := db.UpstreamServer{
			ID:            idgen.NewRandomHex32(),
			Name:          cleanName,
			BaseURL:       cleanURL,
			Username:      cleanUser,
			RemoteUserID:  probe.RemoteUserID,
			AccessToken:   encToken,
			Priority:      priority,
			StreamMode:    mode,
			Enabled:       enabled,
			ServerVersion: probe.Version,
			IsOnline:      probe.Online,
			LatencyMs:     probe.LatencyMs,
		}
		if err := p.db.UpsertServer(ctx, srv); err != nil {
			return nil, fmt.Errorf("insert upstream server: %w", err)
		}
		return &srv, nil
	}

	existing, err := p.db.GetServerByID(ctx, id)
	if err != nil {
		return nil, err
	}

	remoteUserID := existing.RemoteUserID
	encToken := existing.AccessToken
	serverVersion := existing.ServerVersion
	isOnline := existing.IsOnline
	latencyMs := existing.LatencyMs
	lastError := existing.LastError

	if password != "" {
		probe, err := p.TestConnection(ctx, cleanURL, cleanUser, password)
		if err != nil {
			return nil, err
		}
		encToken, err = p.EncryptToken(probe.AccessToken)
		if err != nil {
			return nil, fmt.Errorf("encrypt access token: %w", err)
		}
		remoteUserID = probe.RemoteUserID
		serverVersion = probe.Version
		isOnline = probe.Online
		latencyMs = probe.LatencyMs
		lastError = ""
	}

	updated := db.UpstreamServer{
		ID:            id,
		Name:          cleanName,
		BaseURL:       cleanURL,
		Username:      cleanUser,
		RemoteUserID:  remoteUserID,
		AccessToken:   encToken,
		Priority:      priority,
		StreamMode:    mode,
		Enabled:       enabled,
		ServerVersion: serverVersion,
		IsOnline:      isOnline,
		LatencyMs:     latencyMs,
		LastSyncAt:    existing.LastSyncAt,
		LastError:     lastError,
	}
	if err := p.db.UpdateServer(ctx, updated); err != nil {
		return nil, fmt.Errorf("update upstream server: %w", err)
	}
	if password != "" {
		if err := p.db.UpdateServerStatus(ctx, id, isOnline, latencyMs, serverVersion, ""); err != nil {
			p.logger.Warn("failed to update upstream server probe status", "server_id", id, "err", err)
		}
	}
	return &updated, nil
}

// ListServers returns all configured upstream servers (with stored token values left encrypted).
func (p *Pool) ListServers(ctx context.Context) ([]db.UpstreamServer, error) {
	return p.db.ListServers(ctx)
}

// GetDecryptedServer loads a single upstream server by ID and decrypts its access token.
func (p *Pool) GetDecryptedServer(ctx context.Context, id string) (*db.UpstreamServer, error) {
	srv, err := p.db.GetServerByID(ctx, id)
	if err != nil {
		return nil, err
	}
	plain, err := p.DecryptToken(srv.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("decrypt token for server %s: %w", srv.ID, err)
	}
	srv.AccessToken = plain
	return srv, nil
}

// ListEnabledDecryptedServers returns all enabled upstream servers with decrypted access tokens.
func (p *Pool) ListEnabledDecryptedServers(ctx context.Context) ([]db.UpstreamServer, error) {
	servers, err := p.db.ListEnabledServers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range servers {
		plain, err := p.DecryptToken(servers[i].AccessToken)
		if err != nil {
			return nil, fmt.Errorf("decrypt token for server %s: %w", servers[i].ID, err)
		}
		servers[i].AccessToken = plain
	}
	return servers, nil
}

// DeleteServer removes an upstream server by ID.
func (p *Pool) DeleteServer(ctx context.Context, id string) error {
	return p.db.DeleteServer(ctx, id)
}
