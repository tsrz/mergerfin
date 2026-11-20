package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/tsrz/mergerfin/internal/auth"
	"github.com/tsrz/mergerfin/internal/db"
)

const (
	wsKeepAliveInterval = 30 * time.Second
	wsWriteTimeout      = 10 * time.Second
	wsClientSendBuf     = 64
)

type wsOutbound struct {
	MessageType string `json:"MessageType"`
	MessageID   string `json:"MessageId,omitempty"`
	Data        any    `json:"Data"`
}

type wsInbound struct {
	MessageType string `json:"MessageType"`
	Data        any    `json:"Data"`
}

type wsClient struct {
	conn   *websocket.Conn
	send   chan wsOutbound
	cancel context.CancelFunc
	userID string
	token  string
}

// WSHub manages authenticated Jellyfin WebSocket connections and non-blocking broadcasts.
type WSHub struct {
	db      *db.DB
	logger  *slog.Logger
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	closed  bool
	wg      sync.WaitGroup
}

// NewWSHub creates a new WebSocket hub backed by database for session validation.
func NewWSHub(database *db.DB, logger *slog.Logger) *WSHub {
	if logger == nil {
		logger = slog.Default()
	}
	return &WSHub{
		db:      database,
		logger:  logger,
		clients: make(map[*wsClient]struct{}),
	}
}

// HandleWebSocket authenticates the request token against proxy_sessions, upgrades to WebSocket,
// sends initial ForceKeepAlive and KeepAlive frames, and handles keep-alive pings until disconnect.
func (h *WSHub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeWSUnauthorized(w)
		return
	}

	info := auth.ParseRequest(r)
	if info.Token == "" {
		writeWSUnauthorized(w)
		return
	}

	session, err := h.db.GetSessionByToken(r.Context(), info.Token)
	if err != nil {
		writeWSUnauthorized(w)
		return
	}
	_ = h.db.TouchSession(r.Context(), session.Token)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.logger.Debug("websocket accept failed", "error", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	client := &wsClient{
		conn:   conn,
		send:   make(chan wsOutbound, wsClientSendBuf),
		cancel: cancel,
		userID: session.UserID,
		token:  session.Token,
	}

	// Queue initial handshake frames before registering so they are always sent first.
	client.send <- wsOutbound{MessageType: "ForceKeepAlive", Data: 60}
	client.send <- wsOutbound{MessageType: "KeepAlive", Data: ""}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		cancel()
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	h.clients[client] = struct{}{}
	h.wg.Add(1)
	h.mu.Unlock()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		h.writeLoop(ctx, client)
	}()

	defer func() {
		cancel()
		h.removeClient(client)
		_ = conn.Close(websocket.StatusNormalClosure, "")
		<-writerDone
		h.wg.Done()
	}()

	h.readLoop(ctx, client)
}

func (h *WSHub) writeLoop(ctx context.Context, client *wsClient) {
	ticker := time.NewTicker(wsKeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-client.send:
			if !h.writeFrame(ctx, client, msg) {
				return
			}
		case <-ticker.C:
			if !h.writeFrame(ctx, client, wsOutbound{MessageType: "KeepAlive", Data: ""}) {
				return
			}
		}
	}
}

func (h *WSHub) writeFrame(ctx context.Context, client *wsClient, msg wsOutbound) bool {
	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()

	if err := wsjson.Write(writeCtx, client.conn, msg); err != nil {
		h.logger.Debug("websocket write failed", "user_id", client.userID, "error", err)
		client.cancel()
		return false
	}
	return true
}

func (h *WSHub) readLoop(ctx context.Context, client *wsClient) {
	for {
		var msg wsInbound
		if err := wsjson.Read(ctx, client.conn, &msg); err != nil {
			return
		}
		if strings.EqualFold(msg.MessageType, "KeepAlive") {
			select {
			case client.send <- wsOutbound{MessageType: "KeepAlive", Data: ""}:
			case <-ctx.Done():
				return
			default:
			}
		}
	}
}

func (h *WSHub) removeClient(client *wsClient) {
	h.mu.Lock()
	delete(h.clients, client)
	h.mu.Unlock()
}

// Broadcast performs a non-blocking fan-out of a Jellyfin WebSocket message to all connected clients.
func (h *WSHub) Broadcast(messageType string, data any) {
	msg := wsOutbound{
		MessageType: messageType,
		Data:        data,
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.clients {
		select {
		case client.send <- msg:
		default:
			h.logger.Debug("websocket client send buffer full, dropping broadcast",
				"user_id", client.userID,
				"message_type", messageType,
			)
		}
	}
}

// CloseAll closes all active WebSocket connections and waits for their handlers to exit.
func (h *WSHub) CloseAll() {
	h.mu.Lock()
	h.closed = true
	clients := make([]*wsClient, 0, len(h.clients))
	for client := range h.clients {
		clients = append(clients, client)
		delete(h.clients, client)
	}
	h.mu.Unlock()

	for _, client := range clients {
		client.cancel()
		_ = client.conn.Close(websocket.StatusGoingAway, "server shutting down")
	}

	h.wg.Wait()
}

func writeWSUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
}
