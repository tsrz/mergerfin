package syncengine

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/tsrz/mergerfin/internal/upstream"
)

var providerKeyDisplayNames = map[string]string{
	"tmdb":             "Tmdb",
	"imdb":             "Imdb",
	"tvdb":             "Tvdb",
	"tmdbcollection":   "TmdbCollection",
	"musicbrainzalbum": "MusicBrainzAlbum",
}

// NormalizeProviderIDs returns a map with lowercase keys and trimmed non-empty values.
func NormalizeProviderIDs(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		nk := strings.ToLower(strings.TrimSpace(k))
		nv := strings.TrimSpace(v)
		if nk != "" && nv != "" {
			out[nk] = nv
		}
	}
	return out
}

// FormatProviderIDsJSON serializes normalized provider IDs with Jellyfin-standard key casing.
func FormatProviderIDsJSON(norm map[string]string) string {
	if len(norm) == 0 {
		return "{}"
	}
	formatted := make(map[string]string, len(norm))
	for k, v := range norm {
		nk := strings.ToLower(strings.TrimSpace(k))
		nv := strings.TrimSpace(v)
		if nk == "" || nv == "" {
			continue
		}
		if display, ok := providerKeyDisplayNames[nk]; ok {
			formatted[display] = nv
		} else {
			formatted[strings.ToUpper(nk[:1])+nk[1:]] = nv
		}
	}
	if len(formatted) == 0 {
		return "{}"
	}
	data, err := json.Marshal(formatted)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// ResolveMovieKey computes the canonical deduplication key for a movie.
func ResolveMovieKey(name string, year int, providerIDs map[string]string) string {
	norm := NormalizeProviderIDs(providerIDs)
	if id := norm["tmdb"]; id != "" {
		return "movie:tmdb:" + id
	}
	if id := norm["imdb"]; id != "" {
		return "movie:imdb:" + id
	}
	return fmt.Sprintf("movie:slug:%s:year:%d", slugify(name), year)
}

// ResolveSeriesKey computes the canonical deduplication key for a TV series.
func ResolveSeriesKey(name string, providerIDs map[string]string) string {
	norm := NormalizeProviderIDs(providerIDs)
	if id := norm["tmdb"]; id != "" {
		return "series:tmdb:" + id
	}
	if id := norm["tvdb"]; id != "" {
		return "series:tvdb:" + id
	}
	if id := norm["imdb"]; id != "" {
		return "series:imdb:" + id
	}
	return "series:slug:" + slugify(name)
}

// ResolveSeasonKey computes the canonical deduplication key for a TV season.
func ResolveSeasonKey(seriesKey string, seasonIdx int) string {
	return fmt.Sprintf("season:%s:s%d", seriesKey, seasonIdx)
}

// ResolveEpisodeKey computes the canonical deduplication key for a TV episode.
func ResolveEpisodeKey(seriesKey string, parentIdx, epIdx int, epName string, providerIDs map[string]string) string {
	if epIdx > 0 && seriesKey != "" {
		return fmt.Sprintf("episode:%s:s%d:e%d", seriesKey, parentIdx, epIdx)
	}
	norm := NormalizeProviderIDs(providerIDs)
	if id := norm["tmdb"]; id != "" {
		return "episode:tmdb:" + id
	}
	if id := norm["tvdb"]; id != "" {
		return "episode:tvdb:" + id
	}
	if id := norm["imdb"]; id != "" {
		return "episode:imdb:" + id
	}
	return fmt.Sprintf("episode:%s:slug:%s", seriesKey, slugify(epName))
}

// FormatSourceLabel builds a human-readable source label for multi-version selection,
// e.g. "[Upstream A] 1080p H264 (4.2 Mbps)".
func FormatSourceLabel(serverName string, ms upstream.RemoteMediaSource) string {
	var parts []string
	if strings.TrimSpace(serverName) != "" {
		parts = append(parts, "["+strings.TrimSpace(serverName)+"]")
	}

	var height int
	var videoCodec string
	for _, stream := range ms.MediaStreams {
		if strings.EqualFold(stream.Type, "Video") {
			height = stream.Height
			videoCodec = strings.ToUpper(strings.TrimSpace(stream.Codec))
			break
		}
	}

	if res := formatResolution(height); res != "" {
		parts = append(parts, res)
	}
	if videoCodec != "" {
		parts = append(parts, videoCodec)
	} else if c := strings.ToUpper(strings.TrimSpace(ms.Container)); c != "" {
		parts = append(parts, c)
	}

	if ms.Bitrate > 0 {
		parts = append(parts, "("+formatBitrate(ms.Bitrate)+")")
	}

	if len(parts) == 0 {
		if ms.Name != "" {
			return ms.Name
		}
		return "Media Source"
	}
	return strings.Join(parts, " ")
}

func formatResolution(height int) string {
	switch {
	case height >= 2100:
		return "2160p"
	case height >= 1000:
		return "1080p"
	case height >= 700:
		return "720p"
	case height >= 460:
		return "480p"
	case height > 0:
		return fmt.Sprintf("%dp", height)
	default:
		return ""
	}
}

func formatBitrate(bitrate int64) string {
	if bitrate >= 1_000_000 {
		return fmt.Sprintf("%.1f Mbps", float64(bitrate)/1_000_000.0)
	}
	if bitrate >= 1_000 {
		return fmt.Sprintf("%d kbps", bitrate/1_000)
	}
	return fmt.Sprintf("%d bps", bitrate)
}

func slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "untitled"
	}
	return out
}
