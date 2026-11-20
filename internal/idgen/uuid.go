package idgen

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// MergerFinNamespace is the fixed UUID namespace used for deterministic UUIDv5 generation.
var MergerFinNamespace = uuid.MustParse("a19f4d2e-7c8b-5f3a-9e1d-2b4c6d8e0f1a")

// FormatHex32 formats a uuid.UUID as a lowercase 32-character hex string without dashes.
func FormatHex32(u uuid.UUID) string {
	return strings.ReplaceAll(strings.ToLower(u.String()), "-", "")
}

// NormalizeID trims whitespace, removes dashes (supporting 36-char UUIDs or 32-char hex),
// validates that the result is exactly 32 hexadecimal characters, and returns lowercase 32-char hex.
func NormalizeID(raw string) (string, error) {
	cleaned := strings.ReplaceAll(strings.TrimSpace(raw), "-", "")
	if len(cleaned) != 32 {
		return "", fmt.Errorf("invalid ID %q: expected 32 hex characters (got %d)", raw, len(cleaned))
	}
	for i := 0; i < len(cleaned); i++ {
		c := cleaned[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return "", fmt.Errorf("invalid ID %q: non-hex character %q at index %d", raw, c, i)
		}
	}
	return strings.ToLower(cleaned), nil
}

// NewRandomHex32 returns a newly generated random UUIDv4 formatted as 32 lowercase hex characters.
func NewRandomHex32() string {
	return FormatHex32(uuid.New())
}

// DeterministicHex32 generates a deterministic UUIDv5 under MergerFinNamespace formatted as 32 lowercase hex characters.
func DeterministicHex32(name string) string {
	return FormatHex32(uuid.NewSHA1(MergerFinNamespace, []byte(name)))
}

// VirtualItemID returns the deterministic 32-char hex ID for a virtual media item canonical key.
func VirtualItemID(canonicalKey string) string {
	return DeterministicHex32(canonicalKey)
}

// SourceID returns the deterministic 32-char hex ID for a media source on a specific upstream server.
func SourceID(serverID, remoteMediaSourceID string) string {
	return DeterministicHex32("source:" + serverID + ":" + remoteMediaSourceID)
}

// MoviesLibraryID returns the deterministic 32-char hex ID for the virtual Movies library.
func MoviesLibraryID() string {
	return DeterministicHex32("library:movies")
}

// TVShowsLibraryID returns the deterministic 32-char hex ID for the virtual TV Shows library.
func TVShowsLibraryID() string {
	return DeterministicHex32("library:tvshows")
}
