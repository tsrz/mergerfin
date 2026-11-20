package idgen

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func assertHex32(t *testing.T, id string) {
	t.Helper()
	if len(id) != 32 {
		t.Fatalf("expected length 32, got %d (%q)", len(id), id)
	}
	if strings.Contains(id, "-") {
		t.Fatalf("expected no dashes in %q", id)
	}
	if id != strings.ToLower(id) {
		t.Fatalf("expected lowercase hex, got %q", id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("unexpected char %q at index %d in %q", c, i, id)
		}
	}
}

func TestFormatHex32(t *testing.T) {
	u := uuid.MustParse("A19F4D2E-7C8B-5F3A-9E1D-2B4C6D8E0F1A")
	got := FormatHex32(u)
	want := "a19f4d2e7c8b5f3a9e1d2b4c6d8e0f1a"
	if got != want {
		t.Fatalf("FormatHex32() = %q, want %q", got, want)
	}
	assertHex32(t, got)
}

func TestNewRandomHex32(t *testing.T) {
	id1 := NewRandomHex32()
	id2 := NewRandomHex32()
	assertHex32(t, id1)
	assertHex32(t, id2)
	if id1 == id2 {
		t.Fatalf("expected distinct random IDs, got %q twice", id1)
	}
}

func TestDeterministicHex32(t *testing.T) {
	id1 := DeterministicHex32("movie:tmdb:27205")
	id2 := DeterministicHex32("movie:tmdb:27205")
	id3 := DeterministicHex32("movie:tmdb:157336")

	assertHex32(t, id1)
	assertHex32(t, id3)

	if id1 != id2 {
		t.Fatalf("expected deterministic output, got %q and %q", id1, id2)
	}
	if id1 == id3 {
		t.Fatalf("expected distinct outputs for different inputs, got %q", id1)
	}

	// Verify it matches standard UUIDv5 under MergerFinNamespace.
	expected := FormatHex32(uuid.NewSHA1(MergerFinNamespace, []byte("movie:tmdb:27205")))
	if id1 != expected {
		t.Fatalf("DeterministicHex32() = %q, want %q", id1, expected)
	}
}

func TestDerivedIDs(t *testing.T) {
	v1 := VirtualItemID("series:tvdb:81189")
	v2 := DeterministicHex32("series:tvdb:81189")
	if v1 != v2 {
		t.Fatalf("VirtualItemID mismatch: %q != %q", v1, v2)
	}
	assertHex32(t, v1)

	s1 := SourceID("srv1", "ms1")
	s2 := SourceID("srv1", "ms2")
	assertHex32(t, s1)
	assertHex32(t, s2)
	if s1 == s2 {
		t.Fatalf("expected distinct SourceIDs")
	}
	if s1 != DeterministicHex32("source:srv1:ms1") {
		t.Fatalf("SourceID mismatch: got %q", s1)
	}

	movies := MoviesLibraryID()
	tv := TVShowsLibraryID()
	assertHex32(t, movies)
	assertHex32(t, tv)
	if movies == tv {
		t.Fatalf("expected distinct library IDs")
	}
	if movies != DeterministicHex32("library:movies") {
		t.Fatalf("MoviesLibraryID mismatch: %q", movies)
	}
	if tv != DeterministicHex32("library:tvshows") {
		t.Fatalf("TVShowsLibraryID mismatch: %q", tv)
	}
}

func TestNormalizeID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "36-char lowercase uuid",
			input: "a19f4d2e-7c8b-5f3a-9e1d-2b4c6d8e0f1a",
			want:  "a19f4d2e7c8b5f3a9e1d2b4c6d8e0f1a",
		},
		{
			name:  "36-char uppercase uuid with whitespace",
			input: "  A19F4D2E-7C8B-5F3A-9E1D-2B4C6D8E0F1A \n",
			want:  "a19f4d2e7c8b5f3a9e1d2b4c6d8e0f1a",
		},
		{
			name:  "32-char mixed case hex",
			input: "A19F4d2e7c8B5f3a9E1d2b4c6d8e0F1A",
			want:  "a19f4d2e7c8b5f3a9e1d2b4c6d8e0f1a",
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "too short",
			input:   "a19f4d2e7c8b5f3a",
			wantErr: true,
		},
		{
			name:    "too long",
			input:   "a19f4d2e7c8b5f3a9e1d2b4c6d8e0f1a00",
			wantErr: true,
		},
		{
			name:    "non-hex character",
			input:   "g19f4d2e-7c8b-5f3a-9e1d-2b4c6d8e0f1a",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeID(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeID(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
