package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
	"golang.org/x/crypto/bcrypt"
)

// ClientInfo holds Jellyfin client metadata and authentication token extracted from an HTTP request.
type ClientInfo struct {
	Client   string
	Device   string
	DeviceID string
	Version  string
	Token    string
}

// ParseRequest extracts Jellyfin client metadata and authentication token from r
// in strict priority order:
//  1. Authorization header, then X-Emby-Authorization header starting with MediaBrowser or Emby (case-insensitive)
//  2. X-Emby-Token, then X-MediaBrowser-Token, then Authorization: Bearer <token>
//  3. Query param api_key, then ApiKey, then cookie mergerfin_admin_token
func ParseRequest(r *http.Request) ClientInfo {
	var info ClientInfo
	if r == nil {
		return info
	}

	if params, ok := extractAuthSchemeParams(r.Header.Get("Authorization")); ok {
		parseAuthParams(params, &info)
	}
	if params, ok := extractAuthSchemeParams(r.Header.Get("X-Emby-Authorization")); ok {
		var secondary ClientInfo
		parseAuthParams(params, &secondary)
		mergeClientInfo(&info, secondary)
	}

	if info.Token == "" {
		if tok := strings.TrimSpace(r.Header.Get("X-Emby-Token")); tok != "" {
			info.Token = tok
		} else if tok := strings.TrimSpace(r.Header.Get("X-MediaBrowser-Token")); tok != "" {
			info.Token = tok
		} else if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
			info.Token = tok
		}
	}

	if info.Token == "" && r.URL != nil {
		q := r.URL.Query()
		if tok := strings.TrimSpace(q.Get("api_key")); tok != "" {
			info.Token = tok
		} else if tok := strings.TrimSpace(q.Get("ApiKey")); tok != "" {
			info.Token = tok
		}
	}

	if info.Token == "" {
		if c, err := r.Cookie("mergerfin_admin_token"); err == nil {
			if tok := strings.TrimSpace(c.Value); tok != "" {
				info.Token = tok
			}
		}
	}

	return info
}

func extractAuthSchemeParams(headerVal string) (string, bool) {
	trimmed := strings.TrimSpace(headerVal)
	lower := strings.ToLower(trimmed)

	for _, prefix := range []string{"mediabrowser", "emby"} {
		if strings.HasPrefix(lower, prefix) {
			rest := trimmed[len(prefix):]
			if rest == "" {
				return "", true
			}
			if rest[0] == ' ' || rest[0] == '\t' {
				return strings.TrimSpace(rest), true
			}
		}
	}
	return "", false
}

func extractBearerToken(headerVal string) (string, bool) {
	trimmed := strings.TrimSpace(headerVal)
	if len(trimmed) < len("bearer ") {
		return "", false
	}
	if !strings.EqualFold(trimmed[:len("bearer")], "bearer") {
		return "", false
	}
	if trimmed[len("bearer")] != ' ' && trimmed[len("bearer")] != '\t' {
		return "", false
	}
	tok := strings.TrimSpace(trimmed[len("bearer"):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

func parseAuthParams(s string, info *ClientInfo) {
	n := len(s)
	i := 0
	for i < n {
		for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n' || s[i] == ',') {
			i++
		}
		if i >= n {
			break
		}

		keyStart := i
		for i < n && s[i] != '=' && s[i] != ',' {
			i++
		}
		key := strings.TrimSpace(s[keyStart:i])
		if i >= n || s[i] == ',' {
			i++
			continue
		}
		// Skip '=' and leading whitespace before value.
		i++
		for i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}

		var val string
		if i < n && (s[i] == '"' || s[i] == '\'') {
			quote := s[i]
			i++
			var b strings.Builder
			for i < n {
				if s[i] == '\\' && i+1 < n && (s[i+1] == quote || s[i+1] == '\\') {
					b.WriteByte(s[i+1])
					i += 2
					continue
				}
				if s[i] == quote {
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			val = b.String()
			// Advance to next comma if trailing chars exist.
			for i < n && s[i] != ',' {
				i++
			}
		} else {
			valStart := i
			for i < n && s[i] != ',' {
				i++
			}
			val = strings.TrimSpace(s[valStart:i])
		}

		assignClientField(info, key, val)
	}
}

func assignClientField(info *ClientInfo, key, val string) {
	switch strings.ToLower(key) {
	case "client":
		if info.Client == "" {
			info.Client = val
		}
	case "device":
		if info.Device == "" {
			info.Device = val
		}
	case "deviceid":
		if info.DeviceID == "" {
			info.DeviceID = val
		}
	case "version":
		if info.Version == "" {
			info.Version = val
		}
	case "token":
		if info.Token == "" {
			info.Token = val
		}
	}
}

func mergeClientInfo(dst *ClientInfo, src ClientInfo) {
	if dst.Client == "" {
		dst.Client = src.Client
	}
	if dst.Device == "" {
		dst.Device = src.Device
	}
	if dst.DeviceID == "" {
		dst.DeviceID = src.DeviceID
	}
	if dst.Version == "" {
		dst.Version = src.Version
	}
	if dst.Token == "" {
		dst.Token = src.Token
	}
}

// HashPassword hashes password using bcrypt with bcrypt.DefaultCost.
func HashPassword(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hashed), nil
}

// VerifyPassword verifies a plaintext password against a bcrypt hash.
func VerifyPassword(hash, password string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// EnsureAdminUser ensures an admin user with the given username exists in database,
// creating it with a deterministic 32-hex ID and bcrypt-hashed password if missing.
func EnsureAdminUser(ctx context.Context, database *db.DB, username, password string) (*db.ProxyUser, error) {
	u, err := database.GetUserByUsername(ctx, username)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lookup admin user %q: %w", username, err)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}

	newUser := db.ProxyUser{
		ID:           idgen.DeterministicHex32("user:" + strings.ToLower(username)),
		Username:     username,
		PasswordHash: hash,
		IsAdmin:      true,
	}
	if err := database.CreateUser(ctx, newUser); err != nil {
		return nil, fmt.Errorf("create admin user %q: %w", username, err)
	}

	created, err := database.GetUserByID(ctx, newUser.ID)
	if err != nil {
		return nil, fmt.Errorf("fetch created admin user %q: %w", username, err)
	}
	return created, nil
}
