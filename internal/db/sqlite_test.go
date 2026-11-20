package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tsrz/mergerfin/internal/idgen"
)

func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mergerfin.db")
	d, err := Open(context.Background(), dbPath, "TestMergerFin")
	if err != nil {
		t.Fatalf("Open(%q) failed: %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("Close() failed: %v", err)
		}
	})
	return d, dbPath
}

func TestOpenWALForeignKeysAndSeeding(t *testing.T) {
	d, dbPath := openTestDB(t)
	ctx := context.Background()

	var journalMode string
	if err := d.SqlDB().QueryRowContext(ctx, `PRAGMA journal_mode;`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %q", journalMode)
	}

	var fkEnabled int
	if err := d.SqlDB().QueryRowContext(ctx, `PRAGMA foreign_keys;`).Scan(&fkEnabled); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fkEnabled != 1 {
		t.Fatalf("expected foreign_keys=1, got %d", fkEnabled)
	}

	serverID, err := d.GetSetting(ctx, "server_id")
	if err != nil || len(serverID) != 32 {
		t.Fatalf("unexpected server_id=%q err=%v", serverID, err)
	}
	serverName, err := d.GetSetting(ctx, "server_name")
	if err != nil || serverName != "TestMergerFin" {
		t.Fatalf("unexpected server_name=%q err=%v", serverName, err)
	}
	salt, err := d.GetSetting(ctx, "encryption_salt")
	if err != nil || len(salt) != 32 {
		t.Fatalf("unexpected encryption_salt=%q err=%v", salt, err)
	}

	if err := d.SetSetting(ctx, "server_name", "UpdatedName"); err != nil {
		t.Fatalf("SetSetting failed: %v", err)
	}

	libs, err := d.ListLibraries(ctx)
	if err != nil {
		t.Fatalf("ListLibraries failed: %v", err)
	}
	if len(libs) != 2 {
		t.Fatalf("expected 2 seeded libraries, got %d", len(libs))
	}

	moviesLib, err := d.GetLibraryByID(ctx, idgen.MoviesLibraryID())
	if err != nil || moviesLib.CollectionType != "movies" {
		t.Fatalf("unexpected movies library: %+v err=%v", moviesLib, err)
	}
	tvLib, err := d.GetLibraryByID(ctx, idgen.TVShowsLibraryID())
	if err != nil || tvLib.CollectionType != "tvshows" {
		t.Fatalf("unexpected tvshows library: %+v err=%v", tvLib, err)
	}

	// Re-open the same DB file and verify migrations & seeds are idempotent.
	if err := d.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	d2, err := Open(ctx, dbPath, "DifferentDefault")
	if err != nil {
		t.Fatalf("re-open failed: %v", err)
	}
	defer d2.Close()

	gotID, err := d2.GetSetting(ctx, "server_id")
	if err != nil || gotID != serverID {
		t.Fatalf("server_id changed across reopen: %q vs %q (err=%v)", gotID, serverID, err)
	}
	gotName, err := d2.GetSetting(ctx, "server_name")
	if err != nil || gotName != "UpdatedName" {
		t.Fatalf("server_name overwritten across reopen: %q (err=%v)", gotName, err)
	}
}

func TestUserSessionAndDisplayPreferencesCRUD(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	uid := idgen.NewRandomHex32()
	if err := d.CreateUser(ctx, ProxyUser{
		ID:           uid,
		Username:     "alice",
		PasswordHash: "hash1",
		IsAdmin:      true,
	}); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	count, err := d.CountUsers(ctx)
	if err != nil || count != 1 {
		t.Fatalf("expected CountUsers=1, got %d err=%v", count, err)
	}

	byID, err := d.GetUserByID(ctx, uid)
	if err != nil || byID.Username != "alice" || !byID.IsAdmin {
		t.Fatalf("GetUserByID unexpected: %+v err=%v", byID, err)
	}
	byName, err := d.GetUserByUsername(ctx, "alice")
	if err != nil || byName.ID != uid {
		t.Fatalf("GetUserByUsername unexpected: %+v err=%v", byName, err)
	}

	if err := d.UpdateUserPassword(ctx, uid, "hash2"); err != nil {
		t.Fatalf("UpdateUserPassword failed: %v", err)
	}
	users, err := d.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].PasswordHash != "hash2" {
		t.Fatalf("ListUsers unexpected: %+v err=%v", users, err)
	}

	// Display preferences
	if err := d.UpsertDisplayPreferences(ctx, uid, "usersettings", "emby", `{"SortBy":"SortName"}`); err != nil {
		t.Fatalf("UpsertDisplayPreferences failed: %v", err)
	}
	prefs, err := d.GetDisplayPreferences(ctx, uid, "usersettings", "emby")
	if err != nil || prefs != `{"SortBy":"SortName"}` {
		t.Fatalf("GetDisplayPreferences unexpected: %q err=%v", prefs, err)
	}

	// Sessions + FK cascade on user delete
	tok := idgen.NewRandomHex32()
	if err := d.UpsertSession(ctx, ProxySession{
		Token:      tok,
		UserID:     uid,
		DeviceID:   "dev-1",
		ClientName: "Jellyfin Web",
		DeviceName: "Chrome",
		AppVersion: "10.10.0",
	}); err != nil {
		t.Fatalf("UpsertSession failed: %v", err)
	}
	sess, err := d.GetSessionByToken(ctx, tok)
	if err != nil || sess.DeviceID != "dev-1" {
		t.Fatalf("GetSessionByToken unexpected: %+v err=%v", sess, err)
	}
	if err := d.TouchSession(ctx, tok); err != nil {
		t.Fatalf("TouchSession failed: %v", err)
	}

	// FK violation check: session with non-existent user must fail
	err = d.UpsertSession(ctx, ProxySession{
		Token:  idgen.NewRandomHex32(),
		UserID: "nonexistent-user",
	})
	if err == nil {
		t.Fatalf("expected foreign key error for nonexistent user_id")
	}

	// Deleting user cascades to proxy_sessions
	if err := d.DeleteUser(ctx, uid); err != nil {
		t.Fatalf("DeleteUser failed: %v", err)
	}
	_, err = d.GetSessionByToken(ctx, tok)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected session cascade delete (sql.ErrNoRows), got %v", err)
	}

	// DeleteSession directly
	if err := d.DeleteSession(ctx, tok); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
}

func TestUpstreamServerCRUD(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	s1 := UpstreamServer{
		ID:          idgen.NewRandomHex32(),
		Name:        "Upstream A",
		BaseURL:     "http://upstream-a:8096",
		Username:    "admin",
		AccessToken: "tok-a",
		Priority:    10,
		StreamMode:  "proxy",
		Enabled:     true,
		IsOnline:    true,
	}
	s2 := UpstreamServer{
		ID:         idgen.NewRandomHex32(),
		Name:       "Upstream B",
		BaseURL:    "http://upstream-b:8096",
		Username:   "admin",
		Priority:   20,
		StreamMode: "redirect",
		Enabled:    false,
		IsOnline:   false,
	}

	if err := d.UpsertServer(ctx, s1); err != nil {
		t.Fatalf("UpsertServer s1: %v", err)
	}
	if err := d.UpsertServer(ctx, s2); err != nil {
		t.Fatalf("UpsertServer s2: %v", err)
	}

	all, err := d.ListServers(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListServers got %d err=%v", len(all), err)
	}
	enabled, err := d.ListEnabledServers(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].ID != s1.ID {
		t.Fatalf("ListEnabledServers unexpected: %+v err=%v", enabled, err)
	}

	s2.Enabled = true
	s2.Name = "Upstream B Updated"
	if err := d.UpdateServer(ctx, s2); err != nil {
		t.Fatalf("UpdateServer failed: %v", err)
	}
	gotS2, err := d.GetServerByID(ctx, s2.ID)
	if err != nil || !gotS2.Enabled || gotS2.Name != "Upstream B Updated" {
		t.Fatalf("GetServerByID unexpected: %+v err=%v", gotS2, err)
	}

	if err := d.UpdateServerStatus(ctx, s1.ID, true, 42, "10.10.3", ""); err != nil {
		t.Fatalf("UpdateServerStatus failed: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := d.UpdateServerSyncState(ctx, s1.ID, now, ""); err != nil {
		t.Fatalf("UpdateServerSyncState failed: %v", err)
	}
	gotS1, err := d.GetServerByID(ctx, s1.ID)
	if err != nil || gotS1.LatencyMs != 42 || gotS1.ServerVersion != "10.10.3" || gotS1.LastSyncAt == nil {
		t.Fatalf("GetServerByID after status/sync unexpected: %+v err=%v", gotS1, err)
	}

	if err := d.DeleteServer(ctx, s2.ID); err != nil {
		t.Fatalf("DeleteServer failed: %v", err)
	}
	if _, err := d.GetServerByID(ctx, s2.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows after delete, got %v", err)
	}
}

func TestVirtualItemsFTS5SearchAndCascadeDelete(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	srvID := idgen.NewRandomHex32()
	if err := d.UpsertServer(ctx, UpstreamServer{
		ID:       srvID,
		Name:     "Upstream A",
		BaseURL:  "http://a:8096",
		Username: "admin",
		Priority: 10,
		Enabled:  true,
	}); err != nil {
		t.Fatalf("UpsertServer failed: %v", err)
	}

	moviesLib := idgen.MoviesLibraryID()
	interstellarID := idgen.VirtualItemID("movie:tmdb:157336")
	arrivalID := idgen.VirtualItemID("movie:tmdb:329865")

	if err := d.UpsertItem(ctx, VirtualItem{
		ID:              interstellarID,
		LibraryID:       moviesLib,
		ParentID:        moviesLib,
		ItemType:        "Movie",
		CanonicalKey:    "movie:tmdb:157336",
		Name:            "Interstellar",
		SortName:        "Interstellar",
		OriginalTitle:   "Interstellar",
		Overview:        "A team of explorers travel through a wormhole in space.",
		ProductionYear:  2014,
		CommunityRating: 8.7,
		OfficialRating:  "PG-13",
		GenresJSON:      `["Sci-Fi","Drama"]`,
		DateCreated:     "2026-01-02T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertItem Interstellar: %v", err)
	}

	if err := d.UpsertItem(ctx, VirtualItem{
		ID:              arrivalID,
		LibraryID:       moviesLib,
		ParentID:        moviesLib,
		ItemType:        "Movie",
		CanonicalKey:    "movie:tmdb:329865",
		Name:            "Arrival",
		SortName:        "Arrival",
		OriginalTitle:   "Arrival",
		Overview:        "A linguist works with the military to communicate with aliens (foo-bar C++).",
		ProductionYear:  2016,
		CommunityRating: 7.9,
		OfficialRating:  "PG-13",
		GenresJSON:      `["Sci-Fi","Mystery"]`,
		DateCreated:     "2026-01-03T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertItem Arrival: %v", err)
	}

	// FTS5 search for Interstellar (full and prefix)
	for _, term := range []string{"Interstellar", "Inter", "wormhole"} {
		items, total, err := d.QueryItems(ctx, ItemFilter{SearchTerm: term})
		if err != nil {
			t.Fatalf("QueryItems(%q) failed: %v", term, err)
		}
		if total != 1 || len(items) != 1 || items[0].ID != interstellarID {
			t.Fatalf("QueryItems(%q) expected Interstellar, got total=%d items=%+v", term, total, items)
		}
	}

	// Update Interstellar overview and verify FTS5 trigger updates index cleanly
	if err := d.UpsertItem(ctx, VirtualItem{
		ID:              interstellarID,
		LibraryID:       moviesLib,
		ParentID:        moviesLib,
		ItemType:        "Movie",
		CanonicalKey:    "movie:tmdb:157336",
		Name:            "Interstellar",
		SortName:        "Interstellar",
		OriginalTitle:   "Interstellar",
		Overview:        "Gargantua black hole expedition.",
		ProductionYear:  2014,
		CommunityRating: 8.7,
		OfficialRating:  "PG-13",
		GenresJSON:      `["Sci-Fi","Drama"]`,
		DateCreated:     "2026-01-02T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertItem update Interstellar: %v", err)
	}

	_, oldTotal, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "wormhole"})
	if err != nil || oldTotal != 0 {
		t.Fatalf("expected old FTS term 'wormhole' removed after update, got total=%d err=%v", oldTotal, err)
	}
	_, newTotal, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "Gargantua"})
	if err != nil || newTotal != 1 {
		t.Fatalf("expected new FTS term 'Gargantua' indexed after update, got total=%d err=%v", newTotal, err)
	}

	// FTS5 special-character safety (must never fail with SQLite syntax errors)
	specialInputs := []string{
		"foo-bar",
		"C++",
		"O'Brien",
		`"quoted"`,
		"(test)",
		"AND OR NOT NEAR",
		"!!!",
		"---",
		"*:",
		"foo^bar",
		`a"b*c(d)-e+f`,
	}
	for _, raw := range specialInputs {
		if _, _, err := d.QueryItems(ctx, ItemFilter{SearchTerm: raw}); err != nil {
			t.Fatalf("QueryItems special input %q returned error: %v", raw, err)
		}
	}
	fooBarItems, fooBarTotal, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "foo-bar"})
	if err != nil || fooBarTotal != 1 || fooBarItems[0].ID != arrivalID {
		t.Fatalf("expected foo-bar to match Arrival, got total=%d err=%v", fooBarTotal, err)
	}

	// Filters & pagination
	sciFiItems, sciFiTotal, err := d.QueryItems(ctx, ItemFilter{
		ParentID:     moviesLib,
		Recursive:    true,
		IncludeTypes: []string{"Movie"},
		Genres:       []string{"sci-fi"},
		Years:        []int{2016},
	})
	if err != nil || sciFiTotal != 1 || sciFiItems[0].ID != arrivalID {
		t.Fatalf("genre/year filter unexpected: total=%d items=%+v err=%v", sciFiTotal, sciFiItems, err)
	}

	filters, err := d.GetFilters(ctx, moviesLib, []string{"Movie"})
	if err != nil {
		t.Fatalf("GetFilters failed: %v", err)
	}
	if len(filters.Genres) != 3 || len(filters.Years) != 2 || len(filters.OfficialRatings) != 1 {
		t.Fatalf("GetFilters unexpected: %+v", filters)
	}

	sim, err := d.GetSimilarItems(ctx, interstellarID, 5)
	if err != nil || len(sim) != 1 || sim[0].ID != arrivalID {
		t.Fatalf("GetSimilarItems unexpected: %+v err=%v", sim, err)
	}

	// Sources + Cascade delete
	srcID := idgen.SourceID(srvID, "ms-interstellar")
	if err := d.UpsertSource(ctx, ItemSource{
		SourceID:            srcID,
		VirtualItemID:       interstellarID,
		ServerID:            srvID,
		RemoteItemID:        "remote-1",
		RemoteMediaSourceID: "ms-interstellar",
		Label:               "Upstream A (1080p)",
		Container:           "mp4",
		VideoCodec:          "h264",
		AudioCodec:          "aac",
		Width:               1920,
		Height:              1080,
		Bitrate:             5000000,
	}); err != nil {
		t.Fatalf("UpsertSource failed: %v", err)
	}

	byItemMap, err := d.ListSourcesByItemIDs(ctx, []string{interstellarID, arrivalID})
	if err != nil || len(byItemMap[interstellarID]) != 1 {
		t.Fatalf("ListSourcesByItemIDs unexpected: %+v err=%v", byItemMap, err)
	}

	// Deleting the virtual_item cascades to item_sources AND triggers virtual_items_ad
	if _, err := d.SqlDB().ExecContext(ctx, `DELETE FROM virtual_items WHERE id = ?`, interstellarID); err != nil {
		t.Fatalf("delete virtual_item failed: %v", err)
	}
	if _, err := d.GetSourceByID(ctx, srcID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected item_source cascade delete, got err=%v", err)
	}
	_, afterDelTotal, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "Interstellar"})
	if err != nil || afterDelTotal != 0 {
		t.Fatalf("expected FTS row deleted by trigger, got total=%d err=%v", afterDelTotal, err)
	}

	// Test cascade delete when upstream server is deleted
	srcArrivalID := idgen.SourceID(srvID, "ms-arrival")
	if err := d.UpsertSource(ctx, ItemSource{
		SourceID:            srcArrivalID,
		VirtualItemID:       arrivalID,
		ServerID:            srvID,
		RemoteItemID:        "remote-2",
		RemoteMediaSourceID: "ms-arrival",
		Label:               "Upstream A (1080p)",
	}); err != nil {
		t.Fatalf("UpsertSource arrival failed: %v", err)
	}
	if err := d.DeleteServer(ctx, srvID); err != nil {
		t.Fatalf("DeleteServer failed: %v", err)
	}
	sourcesLeft, err := d.ListSourcesByVirtualItemID(ctx, arrivalID)
	if err != nil || len(sourcesLeft) != 0 {
		t.Fatalf("expected item_sources deleted on server delete, got %d err=%v", len(sourcesLeft), err)
	}

	// DeleteOrphanItems removes Arrival (now with 0 sources)
	deleted, err := d.DeleteOrphanItems(ctx)
	if err != nil || deleted != 1 {
		t.Fatalf("expected DeleteOrphanItems=1, got %d err=%v", deleted, err)
	}
}

func TestSeriesEpisodesUserDataNextUpAndPlaySessions(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	tvLib := idgen.TVShowsLibraryID()
	userID := idgen.NewRandomHex32()
	srvID := idgen.NewRandomHex32()
	if err := d.UpsertServer(ctx, UpstreamServer{
		ID:       srvID,
		Name:     "Upstream A",
		BaseURL:  "http://a:8096",
		Username: "admin",
		Enabled:  true,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}

	seriesID := idgen.VirtualItemID("series:tmdb:95396")
	seasonID := idgen.VirtualItemID("season:tmdb:95396:1")
	ep1ID := idgen.VirtualItemID("episode:tmdb:95396:1:1")
	ep2ID := idgen.VirtualItemID("episode:tmdb:95396:1:2")

	for _, it := range []VirtualItem{
		{ID: seriesID, LibraryID: tvLib, ParentID: tvLib, ItemType: "Series", CanonicalKey: "series:tmdb:95396", Name: "Severance", SortName: "Severance"},
		{ID: seasonID, LibraryID: tvLib, ParentID: seriesID, SeriesID: seriesID, ItemType: "Season", CanonicalKey: "season:tmdb:95396:1", Name: "Season 1", IndexNumber: 1, SeriesName: "Severance"},
		{ID: ep1ID, LibraryID: tvLib, ParentID: seasonID, SeriesID: seriesID, SeasonID: seasonID, ItemType: "Episode", CanonicalKey: "episode:tmdb:95396:1:1", Name: "Good News About Hell", IndexNumber: 1, ParentIndexNumber: 1, SeriesName: "Severance", SeasonName: "Season 1", DateCreated: "2026-01-01T00:00:00Z"},
		{ID: ep2ID, LibraryID: tvLib, ParentID: seasonID, SeriesID: seriesID, SeasonID: seasonID, ItemType: "Episode", CanonicalKey: "episode:tmdb:95396:1:2", Name: "Half Loop", IndexNumber: 2, ParentIndexNumber: 1, SeriesName: "Severance", SeasonName: "Season 1", DateCreated: "2026-01-02T00:00:00Z"},
	} {
		if err := d.UpsertItem(ctx, it); err != nil {
			t.Fatalf("UpsertItem %s: %v", it.Name, err)
		}
	}
	for _, epID := range []string{ep1ID, ep2ID} {
		if err := d.UpsertSource(ctx, ItemSource{
			SourceID:            idgen.SourceID(srvID, "ms-"+epID),
			VirtualItemID:       epID,
			ServerID:            srvID,
			RemoteItemID:        "rem-" + epID,
			RemoteMediaSourceID: "ms-" + epID,
			Label:               "1080p",
		}); err != nil {
			t.Fatalf("UpsertSource %s: %v", epID, err)
		}
	}

	seasons, err := d.GetSeasons(ctx, seriesID)
	if err != nil || len(seasons) != 1 || seasons[0].ID != seasonID {
		t.Fatalf("GetSeasons unexpected: %+v err=%v", seasons, err)
	}
	episodes, err := d.GetEpisodes(ctx, seriesID, seasonID)
	if err != nil || len(episodes) != 2 || episodes[0].ID != ep1ID || episodes[1].ID != ep2ID {
		t.Fatalf("GetEpisodes unexpected: %+v err=%v", episodes, err)
	}

	// Initially 2 unplayed
	unplayed, err := d.CountUnplayedInSubtree(ctx, userID, seriesID)
	if err != nil || unplayed != 2 {
		t.Fatalf("expected 2 unplayed, got %d err=%v", unplayed, err)
	}

	// Mark ep1 played -> ep2 is NextUp, 1 unplayed left
	if err := d.SetPlayed(ctx, userID, ep1ID, true, "2026-01-10T10:00:00Z"); err != nil {
		t.Fatalf("SetPlayed failed: %v", err)
	}
	if err := d.SetFavorite(ctx, userID, ep2ID, true); err != nil {
		t.Fatalf("SetFavorite failed: %v", err)
	}
	if err := d.UpsertProgress(ctx, userID, ep2ID, 123456, false, "2026-01-11T10:00:00Z"); err != nil {
		t.Fatalf("UpsertProgress failed: %v", err)
	}

	unplayed, err = d.CountUnplayedInSubtree(ctx, userID, seriesID)
	if err != nil || unplayed != 1 {
		t.Fatalf("expected 1 unplayed, got %d err=%v", unplayed, err)
	}

	nextUp, err := d.GetNextUpEpisodes(ctx, userID, "", 10)
	if err != nil || len(nextUp) != 1 || nextUp[0].ID != ep2ID {
		t.Fatalf("GetNextUpEpisodes unexpected: %+v err=%v", nextUp, err)
	}

	resume, err := d.GetResumeItems(ctx, userID, tvLib, []string{"Episode"}, 10)
	if err != nil || len(resume) != 1 || resume[0].ID != ep2ID {
		t.Fatalf("GetResumeItems unexpected: %+v err=%v", resume, err)
	}

	latest, err := d.GetLatestItems(ctx, userID, tvLib, []string{"Episode"}, 10)
	if err != nil || len(latest) != 1 || latest[0].ID != ep2ID {
		t.Fatalf("GetLatestItems unexpected: %+v err=%v", latest, err)
	}

	// MergeInitialUserItemData must not lower local progress
	if err := d.MergeInitialUserItemData(ctx, UserItemData{
		UserID:                userID,
		VirtualItemID:         ep2ID,
		Played:                false,
		PlaybackPositionTicks: 500,
		PlayCount:             0,
		IsFavorite:            false,
		LastPlayedAt:          "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("MergeInitialUserItemData failed: %v", err)
	}
	ep2Data, err := d.GetUserItemData(ctx, userID, ep2ID)
	if err != nil || ep2Data.PlaybackPositionTicks != 123456 || !ep2Data.IsFavorite {
		t.Fatalf("MergeInitialUserItemData clobbered progress: %+v err=%v", ep2Data, err)
	}

	batch, err := d.BatchUserItemData(ctx, userID, []string{ep1ID, ep2ID})
	if err != nil || len(batch) != 2 || !batch[ep1ID].Played || !batch[ep2ID].IsFavorite {
		t.Fatalf("BatchUserItemData unexpected: %+v err=%v", batch, err)
	}

	// DeleteOrphanItems must keep Series and Season because episodes still have sources
	deleted, err := d.DeleteOrphanItems(ctx)
	if err != nil || deleted != 0 {
		t.Fatalf("DeleteOrphanItems deleted=%d err=%v, want 0", deleted, err)
	}

	// DeleteServerSourcesNotIn keeps only ep2's source
	keepSrc := idgen.SourceID(srvID, "ms-"+ep2ID)
	removedSrc, err := d.DeleteServerSourcesNotIn(ctx, srvID, []string{keepSrc})
	if err != nil || removedSrc != 1 {
		t.Fatalf("DeleteServerSourcesNotIn removed=%d err=%v, want 1", removedSrc, err)
	}

	// ActivePlaySessions CRUD
	psID := idgen.NewRandomHex32()
	if err := d.CreatePlaySession(ctx, ActivePlaySession{
		PlaySessionID:          psID,
		UserID:                 userID,
		DeviceID:               "dev-tv",
		VirtualItemID:          ep2ID,
		SourceID:               keepSrc,
		ServerID:               srvID,
		RemoteItemID:           "rem-" + ep2ID,
		RemotePlaySessionID:    "remote-ps-1",
		UpstreamTranscodingURL: "/videos/123/master.m3u8",
	}); err != nil {
		t.Fatalf("CreatePlaySession failed: %v", err)
	}
	gotPS, err := d.GetPlaySession(ctx, psID)
	if err != nil || gotPS.RemotePlaySessionID != "remote-ps-1" {
		t.Fatalf("GetPlaySession unexpected: %+v err=%v", gotPS, err)
	}
	latestPS, err := d.GetLatestPlaySessionByDevice(ctx, userID, "dev-tv", ep2ID)
	if err != nil || latestPS.PlaySessionID != psID {
		t.Fatalf("GetLatestPlaySessionByDevice unexpected: %+v err=%v", latestPS, err)
	}
	if err := d.DeletePlaySession(ctx, psID); err != nil {
		t.Fatalf("DeletePlaySession failed: %v", err)
	}
}

func TestConcurrentAccessUnderRace(t *testing.T) {
	d, _ := openTestDB(t)
	ctx := context.Background()

	srvID := idgen.NewRandomHex32()
	if err := d.UpsertServer(ctx, UpstreamServer{
		ID:       srvID,
		Name:     "Concurrent Upstream",
		BaseURL:  "http://concurrent:8096",
		Username: "admin",
		Enabled:  true,
	}); err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}

	moviesLib := idgen.MoviesLibraryID()
	userID := idgen.NewRandomHex32()

	const workers = 10
	const perWorker = 10
	var wg sync.WaitGroup
	errCh := make(chan error, workers*perWorker)

	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				key := fmt.Sprintf("movie:concurrent:%d:%d", w, i)
				itemID := idgen.VirtualItemID(key)
				if err := d.UpsertItem(ctx, VirtualItem{
					ID:             itemID,
					LibraryID:      moviesLib,
					ParentID:       moviesLib,
					ItemType:       "Movie",
					CanonicalKey:   key,
					Name:           fmt.Sprintf("Concurrent Film %d-%d", w, i),
					SortName:       fmt.Sprintf("Concurrent Film %02d-%02d", w, i),
					ProductionYear: 2024,
					GenresJSON:     `["Action"]`,
				}); err != nil {
					errCh <- fmt.Errorf("UpsertItem: %w", err)
					return
				}
				if err := d.UpsertSource(ctx, ItemSource{
					SourceID:            idgen.SourceID(srvID, key),
					VirtualItemID:       itemID,
					ServerID:            srvID,
					RemoteItemID:        key,
					RemoteMediaSourceID: key,
					Label:               "1080p",
				}); err != nil {
					errCh <- fmt.Errorf("UpsertSource: %w", err)
					return
				}
				if err := d.UpsertProgress(ctx, userID, itemID, int64(i*1000), false, "2026-01-01T00:00:00Z"); err != nil {
					errCh <- fmt.Errorf("UpsertProgress: %w", err)
					return
				}
				if _, _, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "Concurrent", Limit: 5}); err != nil {
					errCh <- fmt.Errorf("QueryItems: %w", err)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent error: %v", err)
	}

	_, total, err := d.QueryItems(ctx, ItemFilter{SearchTerm: "Concurrent"})
	if err != nil {
		t.Fatalf("final QueryItems failed: %v", err)
	}
	if total != workers*perWorker {
		t.Fatalf("expected %d items, got %d", workers*perWorker, total)
	}
}
