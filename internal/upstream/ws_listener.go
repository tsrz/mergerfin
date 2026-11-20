package upstream

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/tsrz/mergerfin/internal/db"
)

const (
	defaultReconcileInterval = 15 * time.Second
	defaultDebounceDelay     = 1500 * time.Millisecond
	wsDialTimeout            = 10 * time.Second
	wsWriteTimeout           = 5 * time.Second
)

var defaultBackoffSchedule = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

// DeltaSyncer triggers per-server or full catalog syncs when upstream libraries change.
type DeltaSyncer interface {
	TriggerSync(ctx context.Context) error
	SyncServer(ctx context.Context, srv db.UpstreamServer) error
}

// Broadcaster forwards upstream WebSocket notifications to connected downstream clients.
type Broadcaster interface {
	Broadcast(messageType string, data any)
}

type wsMessage struct {
	MessageType string `json:"MessageType"`
	Data        any    `json:"Data"`
}

type serverWorker struct {
	sig    string
	cancel context.CancelFunc
	done   chan struct{}
}

// ListenerManager maintains resilient WebSocket connections to all enabled upstream Jellyfin
// servers, replying to KeepAlive challenges and triggering debounced delta syncs on updates.
type ListenerManager struct {
	pool              *Pool
	syncer            DeltaSyncer
	ws                Broadcaster
	logger            *slog.Logger
	ReconcileInterval time.Duration
	DebounceDelay     time.Duration
	BackoffSchedule   []time.Duration
}

// NewListenerManager creates a ListenerManager.
func NewListenerManager(pool *Pool, syncer DeltaSyncer, ws Broadcaster, logger *slog.Logger) *ListenerManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &ListenerManager{
		pool:              pool,
		syncer:            syncer,
		ws:                ws,
		logger:            logger,
		ReconcileInterval: defaultReconcileInterval,
		DebounceDelay:     defaultDebounceDelay,
		BackoffSchedule:   defaultBackoffSchedule,
	}
}

// Run reconciles enabled upstream server listeners on startup and periodically until ctx is canceled,
// waiting for all listener goroutines to shut down cleanly before returning.
func (m *ListenerManager) Run(ctx context.Context) {
	if m.pool == nil {
		return
	}

	reconcileEvery := m.ReconcileInterval
	if reconcileEvery <= 0 {
		reconcileEvery = defaultReconcileInterval
	}

	workers := make(map[string]*serverWorker)

	stopAll := func() {
		for id, w := range workers {
			w.cancel()
			<-w.done
			delete(workers, id)
		}
	}

	reconcile := func() {
		servers, err := m.pool.ListEnabledDecryptedServers(ctx)
		if err != nil {
			if ctx.Err() == nil {
				m.logger.Warn("ws listener failed to list enabled servers", "error", err)
			}
			return
		}

		desired := make(map[string]db.UpstreamServer, len(servers))
		for _, srv := range servers {
			desired[srv.ID] = srv
		}

		// Stop removed, disabled, or reconfigured workers.
		for id, w := range workers {
			srv, ok := desired[id]
			if !ok || w.sig != serverSignature(srv) {
				w.cancel()
				<-w.done
				delete(workers, id)
			}
		}

		// Start missing workers.
		for id, srv := range desired {
			if _, exists := workers[id]; exists {
				continue
			}
			wCtx, wCancel := context.WithCancel(ctx)
			done := make(chan struct{})
			workers[id] = &serverWorker{
				sig:    serverSignature(srv),
				cancel: wCancel,
				done:   done,
			}
			go func(s db.UpstreamServer) {
				defer close(done)
				m.runServerLoop(wCtx, s)
			}(srv)
		}
	}

	reconcile()

	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			stopAll()
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func serverSignature(srv db.UpstreamServer) string {
	return srv.BaseURL + "|" + srv.AccessToken + "|" + srv.RemoteUserID
}

func (m *ListenerManager) runServerLoop(ctx context.Context, srv db.UpstreamServer) {
	schedule := m.BackoffSchedule
	if len(schedule) == 0 {
		schedule = defaultBackoffSchedule
	}

	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		err := m.connectAndListen(ctx, srv)
		if ctx.Err() != nil {
			return
		}

		if time.Since(started) > 30*time.Second {
			attempt = 0
		}
		delay := schedule[min(attempt, len(schedule)-1)]
		attempt++

		m.logger.Debug("upstream websocket disconnected, backing off",
			"server_id", srv.ID,
			"server_name", srv.Name,
			"backoff", delay,
			"error", err,
		)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func buildWebSocketURL(srv db.UpstreamServer) (string, string, error) {
	cleanBase, err := normalizeBaseURL(srv.BaseURL)
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(cleanBase)
	if err != nil {
		return "", "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	u.Path = strings.TrimRight(u.Path, "/") + "/socket"
	deviceID := "mergerfin-listener-" + srv.ID
	q := u.Query()
	q.Set("ApiKey", srv.AccessToken)
	q.Set("api_key", srv.AccessToken)
	q.Set("deviceId", deviceID)
	u.RawQuery = q.Encode()

	authHdr := fmt.Sprintf(
		`MediaBrowser Client="MergerFin", Device="MergerFin Proxy", DeviceId="%s", Version="12.4.0", Token="%s"`,
		deviceID,
		srv.AccessToken,
	)
	return u.String(), authHdr, nil
}

func (m *ListenerManager) connectAndListen(ctx context.Context, srv db.UpstreamServer) error {
	wsURL, authHdr, err := buildWebSocketURL(srv)
	if err != nil {
		return err
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, wsDialTimeout)
	defer dialCancel()

	headers := make(http.Header)
	headers.Set("Authorization", authHdr)

	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	if err != nil {
		return err
	}
	defer conn.CloseNow()

	debounce := m.DebounceDelay
	if debounce <= 0 {
		debounce = defaultDebounceDelay
	}

	triggerCh := make(chan struct{}, 1)
	debounceCtx, debounceCancel := context.WithCancel(ctx)
	debounceDone := make(chan struct{})
	go func() {
		defer close(debounceDone)
		m.runDebounceSync(debounceCtx, srv, debounce, triggerCh)
	}()
	defer func() {
		debounceCancel()
		<-debounceDone
	}()

	for {
		var msg wsMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return err
		}

		switch strings.ToLower(strings.TrimSpace(msg.MessageType)) {
		case "forcekeepalive", "keepalive":
			writeCtx, writeCancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := wsjson.Write(writeCtx, conn, wsMessage{
				MessageType: "KeepAlive",
				Data:        "",
			})
			writeCancel()
			if err != nil {
				return err
			}

		case "librarychanged", "userdatachanged":
			select {
			case triggerCh <- struct{}{}:
			default:
			}
			if m.ws != nil {
				m.ws.Broadcast(msg.MessageType, msg.Data)
			}
		}
	}
}

func (m *ListenerManager) runDebounceSync(ctx context.Context, srv db.UpstreamServer, delay time.Duration, triggerCh <-chan struct{}) {
	var timer *time.Timer
	var timerC <-chan time.Time

	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer = nil
			timerC = nil
		}
	}
	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			return
		case <-triggerCh:
			if timer == nil {
				timer = time.NewTimer(delay)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(delay)
			}
		case <-timerC:
			timer = nil
			timerC = nil
			if m.syncer != nil {
				syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				err := m.syncer.SyncServer(syncCtx, srv)
				if err != nil && ctx.Err() == nil {
					m.logger.Warn("delta SyncServer failed, falling back to TriggerSync",
						"server_id", srv.ID,
						"error", err,
					)
					if trigErr := m.syncer.TriggerSync(syncCtx); trigErr != nil && ctx.Err() == nil {
						m.logger.Warn("fallback TriggerSync failed", "server_id", srv.ID, "error", trigErr)
					}
				}
				cancel()
			}
		}
	}
}
