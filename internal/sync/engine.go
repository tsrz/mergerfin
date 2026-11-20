package syncengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tsrz/mergerfin/internal/admin"
	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"github.com/tsrz/mergerfin/internal/upstream"
)

// Broadcaster pushes real-time notifications (e.g. LibraryChanged) to connected WebSocket clients.
type Broadcaster interface {
	Broadcast(messageType string, data any)
}

// Engine synchronizes media metadata and sources from enabled upstream Jellyfin servers into SQLite.
type Engine struct {
	db     *db.DB
	pool   *upstream.Pool
	client *upstream.Client
	ws     Broadcaster
	logger *slog.Logger

	syncMu  sync.Mutex
	stateMu sync.RWMutex
	status  admin.SyncStatus
}

// NewEngine constructs a catalog sync engine implementing admin.SyncTrigger.
func NewEngine(database *db.DB, pool *upstream.Pool, client *upstream.Client, ws Broadcaster, logger *slog.Logger) *Engine {
	if client == nil {
		client = upstream.NewClient()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		db:     database,
		pool:   pool,
		client: client,
		ws:     ws,
		logger: logger,
	}
}

// Status returns a snapshot of the current/last sync status.
func (e *Engine) Status() admin.SyncStatus {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	out := e.status
	if e.status.LastSyncAt != nil {
		t := *e.status.LastSyncAt
		out.LastSyncAt = &t
	}
	return out
}

func (e *Engine) setRunning(running bool) {
	e.stateMu.Lock()
	e.status.Running = running
	e.stateMu.Unlock()
}

func (e *Engine) finishStatus(lastErr string, itemsIndexed int) {
	now := time.Now().UTC()
	e.stateMu.Lock()
	e.status.Running = false
	e.status.LastSyncAt = &now
	e.status.LastError = lastErr
	e.status.ItemsIndexed = itemsIndexed
	e.stateMu.Unlock()
}

// TriggerSync executes a synchronous mutex-protected full sync across all enabled upstream servers.
func (e *Engine) TriggerSync(ctx context.Context) error {
	if e.db == nil || e.pool == nil {
		return errors.New("sync engine is not initialized")
	}

	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	e.setRunning(true)

	servers, err := e.pool.ListEnabledDecryptedServers(ctx)
	if err != nil {
		e.finishStatus(err.Error(), 0)
		return fmt.Errorf("list enabled servers: %w", err)
	}

	adminUserIDs, err := e.loadAdminUserIDs(ctx)
	if err != nil {
		e.finishStatus(err.Error(), 0)
		return fmt.Errorf("load admin users: %w", err)
	}

	seenItems := make(map[string]*db.VirtualItem)
	var syncErrs []string
	var totalDeletedSources int64
	var totalItemsUpserted int

	for _, srv := range servers {
		stats, err := e.syncOneServer(ctx, srv, adminUserIDs, seenItems, false)
		if err != nil {
			msg := fmt.Sprintf("server %s (%s): %v", srv.Name, srv.ID, err)
			syncErrs = append(syncErrs, msg)
			e.logger.Warn("upstream sync failed", "server_id", srv.ID, "server_name", srv.Name, "error", err)
			_ = e.db.UpdateServerStatus(ctx, srv.ID, false, srv.LatencyMs, srv.ServerVersion, err.Error())
			_ = e.db.UpdateServerSyncState(ctx, srv.ID, time.Now().UTC(), err.Error())
			continue
		}
		totalDeletedSources += stats.deletedSources
		totalItemsUpserted += stats.itemsUpserted
	}

	orphansDeleted, err := e.db.DeleteOrphanItems(ctx)
	if err != nil {
		syncErrs = append(syncErrs, fmt.Sprintf("prune orphans: %v", err))
		e.logger.Warn("delete orphan items failed", "error", err)
	}

	_, totalCount, countErr := e.db.QueryItems(ctx, db.ItemFilter{Limit: 1})
	if countErr != nil {
		totalCount = len(seenItems)
	}

	lastErr := strings.Join(syncErrs, "; ")
	e.finishStatus(lastErr, totalCount)

	if e.ws != nil && (totalItemsUpserted > 0 || totalDeletedSources > 0 || orphansDeleted > 0) {
		e.ws.Broadcast("LibraryChanged", map[string]any{
			"ItemsAdded":         []string{},
			"ItemsUpdated":       []string{},
			"ItemsRemoved":       []string{},
			"FoldersAddedTo":     []string{},
			"FoldersRemovedFrom": []string{},
		})
	}

	if len(syncErrs) > 0 {
		return errors.New(lastErr)
	}
	return nil
}

// SyncServer runs an incremental sync for a single upstream server.
func (e *Engine) SyncServer(ctx context.Context, srv db.UpstreamServer) error {
	if e.db == nil {
		return errors.New("sync engine is not initialized")
	}

	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	adminUserIDs, err := e.loadAdminUserIDs(ctx)
	if err != nil {
		return fmt.Errorf("load admin users: %w", err)
	}

	seenItems := make(map[string]*db.VirtualItem)
	stats, err := e.syncOneServer(ctx, srv, adminUserIDs, seenItems, true)
	if err != nil {
		_ = e.db.UpdateServerStatus(ctx, srv.ID, false, srv.LatencyMs, srv.ServerVersion, err.Error())
		_ = e.db.UpdateServerSyncState(ctx, srv.ID, time.Now().UTC(), err.Error())
		return err
	}

	orphansDeleted, err := e.db.DeleteOrphanItems(ctx)
	if err != nil {
		return fmt.Errorf("delete orphan items: %w", err)
	}

	if e.ws != nil && (stats.itemsUpserted > 0 || stats.deletedSources > 0 || orphansDeleted > 0) {
		e.ws.Broadcast("LibraryChanged", map[string]any{
			"ItemsAdded":   []string{},
			"ItemsUpdated": []string{},
			"ItemsRemoved": []string{},
		})
	}
	return nil
}

type serverSyncStats struct {
	itemsUpserted  int
	deletedSources int64
}

func (e *Engine) loadAdminUserIDs(ctx context.Context) ([]string, error) {
	users, err := e.db.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, u := range users {
		if u.IsAdmin {
			ids = append(ids, u.ID)
		}
	}
	return ids, nil
}

func (e *Engine) syncOneServer(
	ctx context.Context,
	srv db.UpstreamServer,
	adminUserIDs []string,
	seenItems map[string]*db.VirtualItem,
	checkExistingInDB bool,
) (serverSyncStats, error) {
	var stats serverSyncStats

	start := time.Now()
	allItems, err := e.client.FetchAllItems(ctx, srv, []string{"Movie", "Series", "Season", "Episode"})
	if err != nil {
		return stats, err
	}
	latencyMs := time.Since(start).Milliseconds()

	var movies, series, seasons, episodes []upstream.RemoteItem
	for _, it := range allItems {
		switch {
		case strings.EqualFold(it.Type, "Movie"):
			movies = append(movies, it)
		case strings.EqualFold(it.Type, "Series"):
			series = append(series, it)
		case strings.EqualFold(it.Type, "Season"):
			seasons = append(seasons, it)
		case strings.EqualFold(it.Type, "Episode"):
			episodes = append(episodes, it)
		}
	}

	moviesLibID := idgen.MoviesLibraryID()
	tvLibID := idgen.TVShowsLibraryID()

	seriesKeyByRemoteID := make(map[string]string, len(series))
	seriesNameByKey := make(map[string]string, len(series))
	seasonKeyByRemoteID := make(map[string]string, len(seasons))
	seasonByRemoteID := make(map[string]upstream.RemoteItem, len(seasons))

	var keepSourceIDs []string

	// 1. Series
	for _, it := range series {
		normIDs := NormalizeProviderIDs(it.ProviderIDs)
		seriesKey := ResolveSeriesKey(it.Name, normIDs)
		seriesVID := idgen.VirtualItemID(seriesKey)

		seriesKeyByRemoteID[it.ID] = seriesKey
		seriesNameByKey[seriesKey] = it.Name

		vItem := remoteToVirtualItem(it, srv.ID, seriesVID, tvLibID, tvLibID, "", "", "Series", seriesKey, normIDs)
		if err := e.saveMergedItem(ctx, vItem, seenItems, checkExistingInDB); err != nil {
			return stats, err
		}
		stats.itemsUpserted++
		e.mergeUserData(ctx, adminUserIDs, seriesVID, it.UserData)
	}

	// 2. Seasons
	for _, it := range seasons {
		seriesKey := seriesKeyByRemoteID[it.SeriesID]
		if seriesKey == "" && it.ParentID != "" {
			seriesKey = seriesKeyByRemoteID[it.ParentID]
		}
		if seriesKey == "" {
			seriesKey = ResolveSeriesKey(it.SeriesName, nil)
		}
		seriesVID := idgen.VirtualItemID(seriesKey)

		if err := e.ensurePlaceholderSeries(ctx, seriesVID, seriesKey, tvLibID, it.SeriesName, seriesNameByKey, seenItems, checkExistingInDB); err != nil {
			return stats, err
		}

		seasonIdx := it.IndexNumber
		seasonKey := ResolveSeasonKey(seriesKey, seasonIdx)
		seasonVID := idgen.VirtualItemID(seasonKey)

		seasonKeyByRemoteID[it.ID] = seasonKey
		seasonByRemoteID[it.ID] = it

		vItem := remoteToVirtualItem(it, srv.ID, seasonVID, tvLibID, seriesVID, seriesVID, "", "Season", seasonKey, NormalizeProviderIDs(it.ProviderIDs))
		if vItem.Name == "" {
			vItem.Name = fmt.Sprintf("Season %d", seasonIdx)
		}
		if vItem.SeriesName == "" {
			vItem.SeriesName = seriesNameByKey[seriesKey]
		}
		if err := e.saveMergedItem(ctx, vItem, seenItems, checkExistingInDB); err != nil {
			return stats, err
		}
		stats.itemsUpserted++
		e.mergeUserData(ctx, adminUserIDs, seasonVID, it.UserData)
	}

	// 3. Episodes
	for _, it := range episodes {
		seriesKey := seriesKeyByRemoteID[it.SeriesID]
		if seriesKey == "" {
			seriesKey = ResolveSeriesKey(it.SeriesName, nil)
		}
		seriesVID := idgen.VirtualItemID(seriesKey)

		if err := e.ensurePlaceholderSeries(ctx, seriesVID, seriesKey, tvLibID, it.SeriesName, seriesNameByKey, seenItems, checkExistingInDB); err != nil {
			return stats, err
		}

		seasonIdx := it.ParentIndexNumber
		if seasonIdx == 0 && it.SeasonID != "" {
			if sItem, ok := seasonByRemoteID[it.SeasonID]; ok {
				seasonIdx = sItem.IndexNumber
			}
		}

		seasonKey := seasonKeyByRemoteID[it.SeasonID]
		if seasonKey == "" {
			seasonKey = ResolveSeasonKey(seriesKey, seasonIdx)
		}
		seasonVID := idgen.VirtualItemID(seasonKey)

		if err := e.ensurePlaceholderSeason(ctx, seasonVID, seasonKey, tvLibID, seriesVID, it.SeasonName, it.SeriesName, seriesNameByKey[seriesKey], seasonIdx, seenItems, checkExistingInDB); err != nil {
			return stats, err
		}

		normIDs := NormalizeProviderIDs(it.ProviderIDs)
		epKey := ResolveEpisodeKey(seriesKey, seasonIdx, it.IndexNumber, it.Name, normIDs)
		epVID := idgen.VirtualItemID(epKey)

		vItem := remoteToVirtualItem(it, srv.ID, epVID, tvLibID, seasonVID, seriesVID, seasonVID, "Episode", epKey, normIDs)
		vItem.ParentIndexNumber = seasonIdx
		if vItem.SeriesName == "" {
			vItem.SeriesName = seriesNameByKey[seriesKey]
		}
		if vItem.SeasonName == "" {
			vItem.SeasonName = fmt.Sprintf("Season %d", seasonIdx)
		}

		if err := e.saveMergedItem(ctx, vItem, seenItems, checkExistingInDB); err != nil {
			return stats, err
		}
		stats.itemsUpserted++

		srcIDs, err := e.upsertItemSources(ctx, srv, epVID, it)
		if err != nil {
			return stats, err
		}
		keepSourceIDs = append(keepSourceIDs, srcIDs...)
		e.mergeUserData(ctx, adminUserIDs, epVID, it.UserData)
	}

	// 4. Movies
	for _, it := range movies {
		normIDs := NormalizeProviderIDs(it.ProviderIDs)
		movieKey := ResolveMovieKey(it.Name, it.ProductionYear, normIDs)
		movieVID := idgen.VirtualItemID(movieKey)

		vItem := remoteToVirtualItem(it, srv.ID, movieVID, moviesLibID, moviesLibID, "", "", "Movie", movieKey, normIDs)
		if err := e.saveMergedItem(ctx, vItem, seenItems, checkExistingInDB); err != nil {
			return stats, err
		}
		stats.itemsUpserted++

		srcIDs, err := e.upsertItemSources(ctx, srv, movieVID, it)
		if err != nil {
			return stats, err
		}
		keepSourceIDs = append(keepSourceIDs, srcIDs...)
		e.mergeUserData(ctx, adminUserIDs, movieVID, it.UserData)
	}

	deleted, err := e.db.DeleteServerSourcesNotIn(ctx, srv.ID, keepSourceIDs)
	if err != nil {
		return stats, fmt.Errorf("prune stale sources for server %s: %w", srv.ID, err)
	}
	stats.deletedSources = deleted

	now := time.Now().UTC()
	_ = e.db.UpdateServerStatus(ctx, srv.ID, true, latencyMs, srv.ServerVersion, "")
	if err := e.db.UpdateServerSyncState(ctx, srv.ID, now, ""); err != nil {
		return stats, fmt.Errorf("update server sync state: %w", err)
	}

	return stats, nil
}

func (e *Engine) ensurePlaceholderSeries(
	ctx context.Context,
	seriesVID, seriesKey, tvLibID, seriesName string,
	seriesNameByKey map[string]string,
	seenItems map[string]*db.VirtualItem,
	checkExistingInDB bool,
) error {
	if _, ok := seenItems[seriesVID]; ok {
		return nil
	}
	if strings.TrimSpace(seriesName) == "" {
		seriesName = seriesNameByKey[seriesKey]
	}
	if strings.TrimSpace(seriesName) == "" {
		seriesName = "Unknown Series"
	}
	if seriesNameByKey[seriesKey] == "" {
		seriesNameByKey[seriesKey] = seriesName
	}

	placeholder := db.VirtualItem{
		ID:              seriesVID,
		LibraryID:       tvLibID,
		ParentID:        tvLibID,
		ItemType:        "Series",
		CanonicalKey:    seriesKey,
		Name:            seriesName,
		SortName:        seriesName,
		GenresJSON:      "[]",
		StudiosJSON:     "[]",
		PeopleJSON:      "[]",
		ProviderIDsJSON: "{}",
	}
	return e.saveMergedItem(ctx, placeholder, seenItems, checkExistingInDB)
}

func (e *Engine) ensurePlaceholderSeason(
	ctx context.Context,
	seasonVID, seasonKey, tvLibID, seriesVID, seasonName, itemSeriesName, mappedSeriesName string,
	seasonIdx int,
	seenItems map[string]*db.VirtualItem,
	checkExistingInDB bool,
) error {
	if _, ok := seenItems[seasonVID]; ok {
		return nil
	}
	if strings.TrimSpace(seasonName) == "" {
		seasonName = fmt.Sprintf("Season %d", seasonIdx)
	}
	sName := strings.TrimSpace(itemSeriesName)
	if sName == "" {
		sName = mappedSeriesName
	}

	placeholder := db.VirtualItem{
		ID:              seasonVID,
		LibraryID:       tvLibID,
		ParentID:        seriesVID,
		SeriesID:        seriesVID,
		ItemType:        "Season",
		CanonicalKey:    seasonKey,
		Name:            seasonName,
		SortName:        fmt.Sprintf("%04d", seasonIdx),
		IndexNumber:     seasonIdx,
		SeriesName:      sName,
		GenresJSON:      "[]",
		StudiosJSON:     "[]",
		PeopleJSON:      "[]",
		ProviderIDsJSON: "{}",
	}
	return e.saveMergedItem(ctx, placeholder, seenItems, checkExistingInDB)
}

func (e *Engine) saveMergedItem(
	ctx context.Context,
	incoming db.VirtualItem,
	seenItems map[string]*db.VirtualItem,
	checkExistingInDB bool,
) error {
	if existing, ok := seenItems[incoming.ID]; ok {
		if mergeMissingFields(existing, incoming) {
			return e.db.UpsertItem(ctx, *existing)
		}
		return nil
	}

	if checkExistingInDB {
		if existing, err := e.db.GetItemByID(ctx, incoming.ID); err == nil && existing != nil {
			mergeMissingFields(existing, incoming)
			seenItems[incoming.ID] = existing
			return e.db.UpsertItem(ctx, *existing)
		}
	}

	copyItem := incoming
	seenItems[incoming.ID] = &copyItem
	return e.db.UpsertItem(ctx, incoming)
}

func mergeMissingFields(dst *db.VirtualItem, src db.VirtualItem) bool {
	changed := false
	fillStr := func(target *string, candidate string) {
		if *target == "" && candidate != "" {
			*target = candidate
			changed = true
		}
	}

	fillStr(&dst.Name, src.Name)
	fillStr(&dst.SortName, src.SortName)
	fillStr(&dst.OriginalTitle, src.OriginalTitle)
	fillStr(&dst.Overview, src.Overview)
	fillStr(&dst.Tagline, src.Tagline)
	fillStr(&dst.PremiereDate, src.PremiereDate)
	fillStr(&dst.OfficialRating, src.OfficialRating)
	fillStr(&dst.SeriesName, src.SeriesName)
	fillStr(&dst.SeasonName, src.SeasonName)
	fillStr(&dst.DateCreated, src.DateCreated)

	if dst.ProductionYear == 0 && src.ProductionYear > 0 {
		dst.ProductionYear = src.ProductionYear
		changed = true
	}
	if dst.CommunityRating == 0 && src.CommunityRating > 0 {
		dst.CommunityRating = src.CommunityRating
		changed = true
	}
	if dst.RunTimeTicks == 0 && src.RunTimeTicks > 0 {
		dst.RunTimeTicks = src.RunTimeTicks
		changed = true
	}
	if (dst.GenresJSON == "" || dst.GenresJSON == "[]") && src.GenresJSON != "" && src.GenresJSON != "[]" {
		dst.GenresJSON = src.GenresJSON
		changed = true
	}
	if (dst.StudiosJSON == "" || dst.StudiosJSON == "[]") && src.StudiosJSON != "" && src.StudiosJSON != "[]" {
		dst.StudiosJSON = src.StudiosJSON
		changed = true
	}
	if (dst.PeopleJSON == "" || dst.PeopleJSON == "[]") && src.PeopleJSON != "" && src.PeopleJSON != "[]" {
		dst.PeopleJSON = src.PeopleJSON
		changed = true
	}
	if (dst.ProviderIDsJSON == "" || dst.ProviderIDsJSON == "{}") && src.ProviderIDsJSON != "" && src.ProviderIDsJSON != "{}" {
		dst.ProviderIDsJSON = src.ProviderIDsJSON
		changed = true
	}
	if (dst.ImgPrimaryServerID == "" || dst.ImgPrimaryRemoteID == "") && src.ImgPrimaryServerID != "" && src.ImgPrimaryRemoteID != "" {
		dst.ImgPrimaryServerID = src.ImgPrimaryServerID
		dst.ImgPrimaryRemoteID = src.ImgPrimaryRemoteID
		changed = true
	}
	if (dst.ImgBackdropServerID == "" || dst.ImgBackdropRemoteID == "") && src.ImgBackdropServerID != "" && src.ImgBackdropRemoteID != "" {
		dst.ImgBackdropServerID = src.ImgBackdropServerID
		dst.ImgBackdropRemoteID = src.ImgBackdropRemoteID
		changed = true
	}

	return changed
}

func (e *Engine) upsertItemSources(ctx context.Context, srv db.UpstreamServer, virtualItemID string, it upstream.RemoteItem) ([]string, error) {
	var sourceIDs []string
	for _, ms := range it.MediaSources {
		remoteMSID := ms.ID
		if remoteMSID == "" {
			remoteMSID = it.ID
		}
		srcID := idgen.SourceID(srv.ID, remoteMSID)
		sourceIDs = append(sourceIDs, srcID)

		var videoCodec, audioCodec string
		var width, height int
		for _, stream := range ms.MediaStreams {
			if strings.EqualFold(stream.Type, "Video") && videoCodec == "" {
				videoCodec = stream.Codec
				width = stream.Width
				height = stream.Height
			} else if strings.EqualFold(stream.Type, "Audio") && audioCodec == "" {
				audioCodec = stream.Codec
			}
		}

		rawStreams := strings.TrimSpace(string(ms.RawMediaStreamsJSON))
		if rawStreams == "" {
			if len(ms.MediaStreams) > 0 {
				if b, err := json.Marshal(ms.MediaStreams); err == nil {
					rawStreams = string(b)
				}
			}
			if rawStreams == "" {
				rawStreams = "[]"
			}
		}

		rawSource := strings.TrimSpace(string(ms.RawJSON))
		if rawSource == "" {
			if b, err := json.Marshal(ms); err == nil {
				rawSource = string(b)
			} else {
				rawSource = "{}"
			}
		}

		runTimeTicks := ms.RunTimeTicks
		if runTimeTicks == 0 {
			runTimeTicks = it.RunTimeTicks
		}

		src := db.ItemSource{
			SourceID:            srcID,
			VirtualItemID:       virtualItemID,
			ServerID:            srv.ID,
			RemoteItemID:        it.ID,
			RemoteMediaSourceID: remoteMSID,
			Label:               FormatSourceLabel(srv.Name, ms),
			Container:           ms.Container,
			VideoCodec:          videoCodec,
			AudioCodec:          audioCodec,
			Width:               width,
			Height:              height,
			Bitrate:             ms.Bitrate,
			SizeBytes:           ms.Size,
			RunTimeTicks:        runTimeTicks,
			RawMediaStreamsJSON: rawStreams,
			RawMediaSourceJSON:  rawSource,
		}
		if err := e.db.UpsertSource(ctx, src); err != nil {
			return nil, fmt.Errorf("upsert source %s: %w", srcID, err)
		}
	}
	return sourceIDs, nil
}

func (e *Engine) mergeUserData(ctx context.Context, adminUserIDs []string, virtualItemID string, ud *upstream.RemoteUserData) {
	if ud == nil || len(adminUserIDs) == 0 {
		return
	}
	for _, uid := range adminUserIDs {
		_ = e.db.MergeInitialUserItemData(ctx, db.UserItemData{
			UserID:                uid,
			VirtualItemID:         virtualItemID,
			Played:                ud.Played,
			PlaybackPositionTicks: ud.PlaybackPositionTicks,
			PlayCount:             ud.PlayCount,
			IsFavorite:            ud.IsFavorite,
			LastPlayedAt:          ud.LastPlayedDate,
		})
	}
}

func remoteToVirtualItem(
	it upstream.RemoteItem,
	serverID, virtualID, libraryID, parentID, seriesID, seasonID, itemType, canonicalKey string,
	normProviderIDs map[string]string,
) db.VirtualItem {
	sortName := strings.TrimSpace(it.SortName)
	if sortName == "" {
		sortName = it.Name
	}
	tagline := ""
	if len(it.Taglines) > 0 {
		tagline = it.Taglines[0]
	}

	var primaryServerID, primaryRemoteID string
	if it.ImageTags != nil && it.ImageTags["Primary"] != "" {
		primaryServerID = serverID
		primaryRemoteID = it.ID
	}

	var backdropServerID, backdropRemoteID string
	if len(it.BackdropImageTags) > 0 {
		backdropServerID = serverID
		backdropRemoteID = it.ID
	}

	return db.VirtualItem{
		ID:                  virtualID,
		LibraryID:           libraryID,
		ParentID:            parentID,
		SeriesID:            seriesID,
		SeasonID:            seasonID,
		ItemType:            itemType,
		CanonicalKey:        canonicalKey,
		Name:                it.Name,
		SortName:            sortName,
		OriginalTitle:       it.OriginalTitle,
		Overview:            it.Overview,
		Tagline:             tagline,
		ProductionYear:      it.ProductionYear,
		PremiereDate:        it.PremiereDate,
		CommunityRating:     it.CommunityRating,
		OfficialRating:      it.OfficialRating,
		IndexNumber:         it.IndexNumber,
		ParentIndexNumber:   it.ParentIndexNumber,
		RunTimeTicks:        it.RunTimeTicks,
		SeriesName:          it.SeriesName,
		SeasonName:          it.SeasonName,
		GenresJSON:          marshalJSONOrDefault(it.Genres, "[]"),
		StudiosJSON:         marshalJSONOrDefault(it.Studios, "[]"),
		PeopleJSON:          marshalJSONOrDefault(it.People, "[]"),
		ProviderIDsJSON:     FormatProviderIDsJSON(normProviderIDs),
		ImgPrimaryServerID:  primaryServerID,
		ImgPrimaryRemoteID:  primaryRemoteID,
		ImgBackdropServerID: backdropServerID,
		ImgBackdropRemoteID: backdropRemoteID,
		DateCreated:         it.DateCreated,
	}
}

func marshalJSONOrDefault(v any, fallback string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fallback
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return fallback
	}
	return s
}
