package syncengine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/upstream"
)

const (
	defaultProgressThrottle = 10 * time.Second
	watchPropagateTimeout   = 15 * time.Second
)

type progressKey struct {
	userID string
	itemID string
}

type progressState struct {
	sentAt     time.Time
	lastPaused bool
}

// WatchStateManager persists user watch state locally in SQLite and propagates
// playback progress, played/favorite toggles, and WebSocket notifications across upstreams.
type WatchStateManager struct {
	db     *db.DB
	pool   *upstream.Pool
	client *upstream.Client
	ws     Broadcaster
	logger *slog.Logger

	progressThrottle time.Duration
	nowFn            func() time.Time

	throttleMu   sync.Mutex
	lastProgress map[progressKey]progressState

	wg sync.WaitGroup
}

// NewWatchStateManager constructs a WatchStateManager.
func NewWatchStateManager(database *db.DB, pool *upstream.Pool, client *upstream.Client, ws Broadcaster, logger *slog.Logger) *WatchStateManager {
	if client == nil {
		client = upstream.NewClient()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WatchStateManager{
		db:               database,
		pool:             pool,
		client:           client,
		ws:               ws,
		logger:           logger,
		progressThrottle: defaultProgressThrottle,
		nowFn:            time.Now,
		lastProgress:     make(map[progressKey]progressState),
	}
}

// OnPlaying records initial playback start (when positionTicks > 0) and relays /Sessions/Playing upstream.
func (m *WatchStateManager) OnPlaying(ctx context.Context, userID, virtualItemID, playSessionID string, positionTicks int64) error {
	if m.db == nil {
		return errors.New("watch state manager database is nil")
	}

	if positionTicks > 0 {
		played := false
		if existing, err := m.db.GetUserItemData(ctx, userID, virtualItemID); err == nil && existing != nil {
			played = existing.Played
		}
		if err := m.db.UpsertProgress(ctx, userID, virtualItemID, positionTicks, played, m.nowFn().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}

	if playSessionID == "" || m.pool == nil {
		return nil
	}

	ps, err := m.db.GetPlaySession(ctx, playSessionID)
	if err != nil || ps == nil {
		return nil
	}
	srv, err := m.pool.GetDecryptedServer(ctx, ps.ServerID)
	if err != nil || srv == nil {
		return nil
	}

	remoteMSID := m.lookupRemoteMediaSourceID(ctx, ps.SourceID)
	payload, _ := json.Marshal(map[string]any{
		"ItemId":        ps.RemoteItemID,
		"MediaSourceId": remoteMSID,
		"PlaySessionId": ps.RemotePlaySessionID,
		"PositionTicks": positionTicks,
	})
	if err := m.client.ReportPlaying(ctx, *srv, payload); err != nil {
		m.logger.Debug("upstream ReportPlaying failed", "server_id", srv.ID, "error", err)
	}
	return nil
}

// OnProgress updates local SQLite playback position immediately and throttles upstream
// /Sessions/Playing/Progress relays to once per 10s (or immediately on pause-state transitions).
func (m *WatchStateManager) OnProgress(ctx context.Context, userID, virtualItemID, playSessionID string, positionTicks int64, isPaused bool) error {
	if m.db == nil {
		return errors.New("watch state manager database is nil")
	}

	played := false
	if existing, err := m.db.GetUserItemData(ctx, userID, virtualItemID); err == nil && existing != nil {
		played = existing.Played
	}
	if err := m.db.UpsertProgress(ctx, userID, virtualItemID, positionTicks, played, m.nowFn().UTC().Format(time.RFC3339)); err != nil {
		return err
	}

	key := progressKey{userID: userID, itemID: virtualItemID}
	now := m.nowFn()
	shouldRelay := false

	m.throttleMu.Lock()
	prev, exists := m.lastProgress[key]
	if !exists || prev.lastPaused != isPaused || now.Sub(prev.sentAt) >= m.progressThrottle {
		shouldRelay = true
		m.lastProgress[key] = progressState{sentAt: now, lastPaused: isPaused}
	}
	m.throttleMu.Unlock()

	if !shouldRelay || playSessionID == "" || m.pool == nil {
		return nil
	}

	ps, err := m.db.GetPlaySession(ctx, playSessionID)
	if err != nil || ps == nil {
		return nil
	}
	srv, err := m.pool.GetDecryptedServer(ctx, ps.ServerID)
	if err != nil || srv == nil {
		return nil
	}

	remoteMSID := m.lookupRemoteMediaSourceID(ctx, ps.SourceID)
	payload, _ := json.Marshal(map[string]any{
		"ItemId":        ps.RemoteItemID,
		"MediaSourceId": remoteMSID,
		"PlaySessionId": ps.RemotePlaySessionID,
		"PositionTicks": positionTicks,
		"IsPaused":      isPaused,
	})
	if err := m.client.ReportProgress(ctx, *srv, payload); err != nil {
		m.logger.Debug("upstream ReportProgress failed", "server_id", srv.ID, "error", err)
	}
	return nil
}

// OnStopped finalizes local watch state (auto-marking played at >=90% runtime),
// stops active upstream encodings, removes the play session, broadcasts UserDataChanged,
// and asynchronously propagates stopped/played state to all servers owning the item.
func (m *WatchStateManager) OnStopped(ctx context.Context, userID, virtualItemID, playSessionID string, positionTicks int64) error {
	if m.db == nil {
		return errors.New("watch state manager database is nil")
	}

	item, itemErr := m.db.GetItemByID(ctx, virtualItemID)
	existing, udErr := m.db.GetUserItemData(ctx, userID, virtualItemID)
	wasPlayed := udErr == nil && existing != nil && existing.Played

	autoPlayed := itemErr == nil && item != nil && item.RunTimeTicks > 0 && positionTicks >= (item.RunTimeTicks*9)/10
	played := wasPlayed || autoPlayed
	nowStr := m.nowFn().UTC().Format(time.RFC3339)

	if !wasPlayed && autoPlayed {
		if err := m.db.SetPlayed(ctx, userID, virtualItemID, true, nowStr); err != nil {
			return err
		}
	} else {
		if err := m.db.UpsertProgress(ctx, userID, virtualItemID, positionTicks, played, nowStr); err != nil {
			return err
		}
	}

	m.throttleMu.Lock()
	delete(m.lastProgress, progressKey{userID: userID, itemID: virtualItemID})
	m.throttleMu.Unlock()

	var ps *db.ActivePlaySession
	var activeRemoteMSID string
	if playSessionID != "" {
		if loadedPS, err := m.db.GetPlaySession(ctx, playSessionID); err == nil && loadedPS != nil {
			ps = loadedPS
			activeRemoteMSID = m.lookupRemoteMediaSourceID(ctx, ps.SourceID)
			if ps.RemotePlaySessionID != "" && m.pool != nil {
				if srv, err := m.pool.GetDecryptedServer(ctx, ps.ServerID); err == nil && srv != nil {
					if stopErr := m.client.StopActiveEncodings(ctx, *srv, ps.DeviceID, ps.RemotePlaySessionID); stopErr != nil {
						m.logger.Debug("upstream StopActiveEncodings on stop failed", "server_id", srv.ID, "error", stopErr)
					}
				}
			}
			_ = m.db.DeletePlaySession(ctx, playSessionID)
		}
	}

	if ud, err := m.db.GetUserItemData(ctx, userID, virtualItemID); err == nil && ud != nil {
		m.broadcastUserDataChanged(userID, virtualItemID, ud)
	}

	if m.pool != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			bgCtx, cancel := context.WithTimeout(context.Background(), watchPropagateTimeout)
			defer cancel()

			activeStoppedKey := ""
			if ps != nil {
				if srv, err := m.pool.GetDecryptedServer(bgCtx, ps.ServerID); err == nil && srv != nil {
					payload, _ := json.Marshal(map[string]any{
						"ItemId":        ps.RemoteItemID,
						"MediaSourceId": activeRemoteMSID,
						"PlaySessionId": ps.RemotePlaySessionID,
						"PositionTicks": positionTicks,
					})
					if err := m.client.ReportStopped(bgCtx, *srv, payload); err != nil {
						m.logger.Debug("upstream ReportStopped failed", "server_id", srv.ID, "error", err)
					} else {
						activeStoppedKey = ps.ServerID + ":" + ps.RemoteItemID
					}
				}
			}

			targets := m.collectUpstreamTargets(bgCtx, virtualItemID)
			for _, t := range targets {
				if played {
					if err := m.client.MarkPlayed(bgCtx, t.srv, t.remoteItemID, true); err != nil {
						m.logger.Debug("upstream MarkPlayed on stop failed", "server_id", t.srv.ID, "error", err)
					}
					continue
				}
				if activeStoppedKey == t.srv.ID+":"+t.remoteItemID {
					continue
				}
				payload, _ := json.Marshal(map[string]any{
					"ItemId":        t.remoteItemID,
					"MediaSourceId": t.remoteMediaSourceID,
					"PositionTicks": positionTicks,
				})
				if err := m.client.ReportStopped(bgCtx, t.srv, payload); err != nil {
					m.logger.Debug("upstream ReportStopped fan-out failed", "server_id", t.srv.ID, "error", err)
				}
			}
		}()
	}

	return nil
}

// SetPlayed persists played state locally and asynchronously fans out MarkPlayed to all owning upstreams.
func (m *WatchStateManager) SetPlayed(ctx context.Context, userID, virtualItemID string, played bool) (*db.UserItemData, error) {
	if m.db == nil {
		return nil, errors.New("watch state manager database is nil")
	}

	if err := m.db.SetPlayed(ctx, userID, virtualItemID, played, m.nowFn().UTC().Format(time.RFC3339)); err != nil {
		return nil, err
	}

	ud, err := m.db.GetUserItemData(ctx, userID, virtualItemID)
	if err != nil {
		return nil, err
	}

	m.broadcastUserDataChanged(userID, virtualItemID, ud)

	if m.pool != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			bgCtx, cancel := context.WithTimeout(context.Background(), watchPropagateTimeout)
			defer cancel()

			for _, t := range m.collectUpstreamTargets(bgCtx, virtualItemID) {
				if err := m.client.MarkPlayed(bgCtx, t.srv, t.remoteItemID, played); err != nil {
					m.logger.Debug("upstream MarkPlayed fan-out failed", "server_id", t.srv.ID, "error", err)
				}
			}
		}()
	}

	return ud, nil
}

// SetFavorite persists favorite state locally and asynchronously fans out MarkFavorite to all owning upstreams.
func (m *WatchStateManager) SetFavorite(ctx context.Context, userID, virtualItemID string, favorite bool) (*db.UserItemData, error) {
	if m.db == nil {
		return nil, errors.New("watch state manager database is nil")
	}

	if err := m.db.SetFavorite(ctx, userID, virtualItemID, favorite); err != nil {
		return nil, err
	}

	ud, err := m.db.GetUserItemData(ctx, userID, virtualItemID)
	if err != nil {
		return nil, err
	}

	m.broadcastUserDataChanged(userID, virtualItemID, ud)

	if m.pool != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			bgCtx, cancel := context.WithTimeout(context.Background(), watchPropagateTimeout)
			defer cancel()

			for _, t := range m.collectUpstreamTargets(bgCtx, virtualItemID) {
				if err := m.client.MarkFavorite(bgCtx, t.srv, t.remoteItemID, favorite); err != nil {
					m.logger.Debug("upstream MarkFavorite fan-out failed", "server_id", t.srv.ID, "error", err)
				}
			}
		}()
	}

	return ud, nil
}

// Flush waits for all in-flight asynchronous upstream propagation goroutines up to timeout.
func (m *WatchStateManager) Flush(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	if timeout <= 0 {
		<-done
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

type upstreamWatchTarget struct {
	srv                 db.UpstreamServer
	remoteItemID        string
	remoteMediaSourceID string
}

func (m *WatchStateManager) collectUpstreamTargets(ctx context.Context, virtualItemID string) []upstreamWatchTarget {
	sources, err := m.db.ListSourcesByVirtualItemID(ctx, virtualItemID)
	if err != nil || len(sources) == 0 {
		return nil
	}
	servers, err := m.pool.ListEnabledDecryptedServers(ctx)
	if err != nil || len(servers) == 0 {
		return nil
	}

	srvByID := make(map[string]db.UpstreamServer, len(servers))
	for _, s := range servers {
		srvByID[s.ID] = s
	}

	seen := make(map[string]struct{}, len(sources))
	targets := make([]upstreamWatchTarget, 0, len(sources))
	for _, src := range sources {
		srv, ok := srvByID[src.ServerID]
		if !ok {
			continue
		}
		dedupKey := srv.ID + ":" + src.RemoteItemID
		if _, dup := seen[dedupKey]; dup {
			continue
		}
		seen[dedupKey] = struct{}{}
		targets = append(targets, upstreamWatchTarget{
			srv:                 srv,
			remoteItemID:        src.RemoteItemID,
			remoteMediaSourceID: src.RemoteMediaSourceID,
		})
	}
	return targets
}

func (m *WatchStateManager) lookupRemoteMediaSourceID(ctx context.Context, sourceID string) string {
	if sourceID == "" || m.db == nil {
		return ""
	}
	src, err := m.db.GetSourceByID(ctx, sourceID)
	if err != nil || src == nil {
		return ""
	}
	return src.RemoteMediaSourceID
}

func (m *WatchStateManager) broadcastUserDataChanged(userID, virtualItemID string, ud *db.UserItemData) {
	if m.ws == nil || ud == nil {
		return
	}
	m.ws.Broadcast("UserDataChanged", map[string]any{
		"UserId": userID,
		"UserDataList": []map[string]any{
			{
				"ItemId":                virtualItemID,
				"PlaybackPositionTicks": ud.PlaybackPositionTicks,
				"PlayCount":             ud.PlayCount,
				"IsFavorite":            ud.IsFavorite,
				"Played":                ud.Played,
				"Key":                   virtualItemID,
				"LastPlayedDate":        ud.LastPlayedAt,
			},
		},
	})
}
