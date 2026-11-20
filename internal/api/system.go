package api

import (
	"net/http"
	"strconv"
	"strings"
)

const (
	jellyfinVersion     = "12.4.0"
	jellyfinProductName = "Jellyfin Server"
	jellyfinOS          = "Linux"

	defaultBitrateTestSize = 1024 * 1024
	maxBitrateTestSize     = 10 * 1024 * 1024
)

type publicSystemInfo struct {
	LocalAddress           string `json:"LocalAddress"`
	ServerName             string `json:"ServerName"`
	Version                string `json:"Version"`
	ProductName            string `json:"ProductName"`
	OperatingSystem        string `json:"OperatingSystem"`
	ID                     string `json:"Id"`
	StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
}

type systemInfo struct {
	LocalAddress               string `json:"LocalAddress"`
	ServerName                 string `json:"ServerName"`
	Version                    string `json:"Version"`
	ProductName                string `json:"ProductName"`
	OperatingSystem            string `json:"OperatingSystem"`
	OperatingSystemDisplayName string `json:"OperatingSystemDisplayName"`
	ID                         string `json:"Id"`
	StartupWizardCompleted     bool   `json:"StartupWizardCompleted"`
	SupportsLibraryMonitor     bool   `json:"SupportsLibraryMonitor"`
	CanSelfRestart             bool   `json:"CanSelfRestart"`
	CanLaunchWebBrowser        bool   `json:"CanLaunchWebBrowser"`
	HasUpdateAvailable         bool   `json:"HasUpdateAvailable"`
	HasPendingRestart          bool   `json:"HasPendingRestart"`
	IsShuttingDown             bool   `json:"IsShuttingDown"`
	EncoderLocation            string `json:"EncoderLocation"`
	SystemArchitecture         string `json:"SystemArchitecture"`
	WebSocketPortNumber        int    `json:"WebSocketPortNumber"`
	HttpServerPortNumber       int    `json:"HttpServerPortNumber"`
	SupportsHttps              bool   `json:"SupportsHttps"`
}

type systemEndpointInfo struct {
	IsLocal     bool `json:"IsLocal"`
	IsInNetwork bool `json:"IsInNetwork"`
}

type brandingConfiguration struct {
	LoginDisclaimer     string `json:"LoginDisclaimer"`
	CustomCss           string `json:"CustomCss"`
	SplashscreenEnabled bool   `json:"SplashscreenEnabled"`
}

func (h *apiHandler) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(jellyfinProductName))
}

func (h *apiHandler) handleSystemInfoPublic(w http.ResponseWriter, r *http.Request) {
	serverID, serverName := h.serverIdentity(r.Context())
	writeJSON(w, http.StatusOK, publicSystemInfo{
		LocalAddress:           baseURLFromRequest(r, h.defaultPort()),
		ServerName:             serverName,
		Version:                jellyfinVersion,
		ProductName:            jellyfinProductName,
		OperatingSystem:        jellyfinOS,
		ID:                     serverID,
		StartupWizardCompleted: true,
	})
}

func (h *apiHandler) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	serverID, serverName := h.serverIdentity(r.Context())
	portNum := 8096
	if p, err := strconv.Atoi(h.defaultPort()); err == nil && p > 0 {
		portNum = p
	}
	writeJSON(w, http.StatusOK, systemInfo{
		LocalAddress:               baseURLFromRequest(r, h.defaultPort()),
		ServerName:                 serverName,
		Version:                    jellyfinVersion,
		ProductName:                jellyfinProductName,
		OperatingSystem:            jellyfinOS,
		OperatingSystemDisplayName: jellyfinOS,
		ID:                         serverID,
		StartupWizardCompleted:     true,
		SupportsLibraryMonitor:     true,
		CanSelfRestart:             false,
		CanLaunchWebBrowser:        false,
		HasUpdateAvailable:         false,
		HasPendingRestart:          false,
		IsShuttingDown:             false,
		EncoderLocation:            "System",
		SystemArchitecture:         "X64",
		WebSocketPortNumber:        portNum,
		HttpServerPortNumber:       portNum,
		SupportsHttps:              false,
	})
}

func (h *apiHandler) handleSystemEndpoint(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, systemEndpointInfo{
		IsLocal:     true,
		IsInNetwork: true,
	})
}

func (h *apiHandler) handleBrandingConfiguration(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, brandingConfiguration{
		LoginDisclaimer:     "",
		CustomCss:           "",
		SplashscreenEnabled: false,
	})
}

func (h *apiHandler) handleBrandingCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

func (h *apiHandler) handleQuickConnectEnabled(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, false)
}

func (h *apiHandler) handleEmptyArray(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

func (h *apiHandler) handleBitrateTest(w http.ResponseWriter, r *http.Request) {
	size := defaultBitrateTestSize
	rawSize := strings.TrimSpace(r.URL.Query().Get("Size"))
	if rawSize == "" {
		rawSize = strings.TrimSpace(r.URL.Query().Get("size"))
	}
	if rawSize != "" {
		if parsed, err := strconv.Atoi(rawSize); err == nil && parsed > 0 {
			size = min(parsed, maxBitrateTestSize)
		}
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.WriteHeader(http.StatusOK)

	const chunkSize = 32 * 1024
	zeros := make([]byte, chunkSize)
	remaining := size
	for remaining > 0 {
		n := min(remaining, chunkSize)
		if _, err := w.Write(zeros[:n]); err != nil {
			return
		}
		remaining -= n
	}
}
