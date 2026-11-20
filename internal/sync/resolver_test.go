package syncengine

import (
	"encoding/json"
	"testing"

	"github.com/tsrz/mergerfin/internal/upstream"
)

func TestNormalizeAndFormatProviderIDs(t *testing.T) {
	raw := map[string]string{
		" Tmdb ": " 157336 ",
		"IMDB":   "tt0816692",
		"Tvdb":   "  ",
		"":       "ignored",
	}
	norm := NormalizeProviderIDs(raw)
	if len(norm) != 2 || norm["tmdb"] != "157336" || norm["imdb"] != "tt0816692" {
		t.Fatalf("unexpected NormalizeProviderIDs result: %+v", norm)
	}

	jsonStr := FormatProviderIDsJSON(norm)
	var decoded map[string]string
	if err := json.Unmarshal([]byte(jsonStr), &decoded); err != nil {
		t.Fatalf("unmarshal FormatProviderIDsJSON: %v", err)
	}
	if decoded["Tmdb"] != "157336" || decoded["Imdb"] != "tt0816692" {
		t.Fatalf("unexpected formatted provider IDs: %+v", decoded)
	}
	if got := FormatProviderIDsJSON(nil); got != "{}" {
		t.Fatalf("FormatProviderIDsJSON(nil) = %q, want {}", got)
	}
}

func TestResolveCanonicalKeys(t *testing.T) {
	// Movies: tmdb > imdb > slug+year
	if got := ResolveMovieKey("Interstellar", 2014, map[string]string{"Tmdb": "157336", "Imdb": "tt0816692"}); got != "movie:tmdb:157336" {
		t.Fatalf("ResolveMovieKey tmdb = %q", got)
	}
	if got := ResolveMovieKey("Interstellar", 2014, map[string]string{"Imdb": "tt0816692"}); got != "movie:imdb:tt0816692" {
		t.Fatalf("ResolveMovieKey imdb = %q", got)
	}
	if got := ResolveMovieKey("The Matrix: Reloaded!", 2003, nil); got != "movie:slug:the-matrix-reloaded:year:2003" {
		t.Fatalf("ResolveMovieKey slug = %q", got)
	}

	// Series: tmdb > tvdb > imdb > slug
	if got := ResolveSeriesKey("Severance", map[string]string{"Tmdb": "95396", "Tvdb": "371980"}); got != "series:tmdb:95396" {
		t.Fatalf("ResolveSeriesKey tmdb = %q", got)
	}
	if got := ResolveSeriesKey("Severance", map[string]string{"Tvdb": "371980", "Imdb": "tt11280740"}); got != "series:tvdb:371980" {
		t.Fatalf("ResolveSeriesKey tvdb = %q", got)
	}
	if got := ResolveSeriesKey("Severance", map[string]string{"Imdb": "tt11280740"}); got != "series:imdb:tt11280740" {
		t.Fatalf("ResolveSeriesKey imdb = %q", got)
	}
	if got := ResolveSeriesKey("Severance", nil); got != "series:slug:severance" {
		t.Fatalf("ResolveSeriesKey slug = %q", got)
	}

	// Season & Episode
	sKey := "series:tmdb:95396"
	if got := ResolveSeasonKey(sKey, 1); got != "season:series:tmdb:95396:s1" {
		t.Fatalf("ResolveSeasonKey = %q", got)
	}
	if got := ResolveEpisodeKey(sKey, 1, 2, "Half Loop", nil); got != "episode:series:tmdb:95396:s1:e2" {
		t.Fatalf("ResolveEpisodeKey standard = %q", got)
	}
	if got := ResolveEpisodeKey(sKey, 1, 0, "Pilot", map[string]string{"Tvdb": "888"}); got != "episode:tvdb:888" {
		t.Fatalf("ResolveEpisodeKey provider fallback = %q", got)
	}
	if got := ResolveEpisodeKey(sKey, 1, 0, "Special Episode", nil); got != "episode:series:tmdb:95396:slug:special-episode" {
		t.Fatalf("ResolveEpisodeKey slug fallback = %q", got)
	}
}

func TestFormatSourceLabel(t *testing.T) {
	ms := upstream.RemoteMediaSource{
		ID:      "ms-1",
		Bitrate: 4200000,
		MediaStreams: []upstream.RemoteMediaStream{
			{Type: "Video", Height: 1080, Codec: "h264"},
			{Type: "Audio", Codec: "aac"},
		},
	}
	if got := FormatSourceLabel("Upstream A", ms); got != "[Upstream A] 1080p H264 (4.2 Mbps)" {
		t.Fatalf("FormatSourceLabel = %q", got)
	}

	ms4k := upstream.RemoteMediaSource{
		ID:      "ms-2",
		Bitrate: 850000,
		MediaStreams: []upstream.RemoteMediaStream{
			{Type: "Video", Height: 2160, Codec: "hevc"},
		},
	}
	if got := FormatSourceLabel("Upstream B", ms4k); got != "[Upstream B] 2160p HEVC (850 kbps)" {
		t.Fatalf("FormatSourceLabel 4K = %q", got)
	}
}
