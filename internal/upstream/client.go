package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tsrz/mergerfin/internal/db"
)

const (
	clientHTTPTimeout = 30 * time.Second
	defaultPageSize   = 200
	itemQueryFields   = "ProviderIds,MediaSources,Overview,Genres,Studios,People,Path,DateCreated,MediaStreams,Taglines,OriginalTitle,SortName,ProductionYear,PremiereDate,CommunityRating,OfficialRating"
)

// Client performs authenticated HTTP calls against upstream Jellyfin 12.4 servers.
type Client struct {
	httpClient *http.Client
}

// NewClient creates an upstream Jellyfin HTTP client with a 30s timeout.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: clientHTTPTimeout,
		},
	}
}

// NameID represents a Jellyfin Name/Id pair (e.g. Studios).
type NameID struct {
	Name string `json:"Name"`
	ID   string `json:"Id,omitempty"`
}

// PersonInfo represents a Jellyfin person credit.
type PersonInfo struct {
	Name            string `json:"Name"`
	ID              string `json:"Id,omitempty"`
	Role            string `json:"Role,omitempty"`
	Type            string `json:"Type,omitempty"`
	PrimaryImageTag string `json:"PrimaryImageTag,omitempty"`
}

// RemoteMediaStream represents a stream entry inside an upstream MediaSource.
type RemoteMediaStream struct {
	Codec        string `json:"Codec,omitempty"`
	CodecTag     string `json:"CodecTag,omitempty"`
	Language     string `json:"Language,omitempty"`
	Title        string `json:"Title,omitempty"`
	DisplayTitle string `json:"DisplayTitle,omitempty"`
	Type         string `json:"Type"`
	Index        int    `json:"Index"`
	IsDefault    bool   `json:"IsDefault"`
	IsForced     bool   `json:"IsForced"`
	IsExternal   bool   `json:"IsExternal"`
	Height       int    `json:"Height,omitempty"`
	Width        int    `json:"Width,omitempty"`
	BitRate      int64  `json:"BitRate,omitempty"`
	Profile      string `json:"Profile,omitempty"`
	AspectRatio  string `json:"AspectRatio,omitempty"`
	PixelFormat  string `json:"PixelFormat,omitempty"`
	Channels     int    `json:"Channels,omitempty"`
	SampleRate   int    `json:"SampleRate,omitempty"`
}

// RemoteMediaSource represents a Jellyfin MediaSourceInfo, preserving raw JSON payloads.
type RemoteMediaSource struct {
	ID                         string              `json:"Id"`
	Name                       string              `json:"Name,omitempty"`
	Path                       string              `json:"Path,omitempty"`
	Protocol                   string              `json:"Protocol,omitempty"`
	Container                  string              `json:"Container,omitempty"`
	Size                       int64               `json:"Size,omitempty"`
	Bitrate                    int64               `json:"Bitrate,omitempty"`
	RunTimeTicks               int64               `json:"RunTimeTicks,omitempty"`
	SupportsDirectPlay         bool                `json:"SupportsDirectPlay"`
	SupportsDirectStream       bool                `json:"SupportsDirectStream"`
	SupportsTranscoding        bool                `json:"SupportsTranscoding"`
	DirectStreamURL            string              `json:"DirectStreamUrl,omitempty"`
	TranscodingURL             string              `json:"TranscodingUrl,omitempty"`
	TranscodingSubProtocol     string              `json:"TranscodingSubProtocol,omitempty"`
	TranscodingContainer       string              `json:"TranscodingContainer,omitempty"`
	MediaStreams               []RemoteMediaStream `json:"MediaStreams,omitempty"`
	DefaultAudioStreamIndex    *int                `json:"DefaultAudioStreamIndex,omitempty"`
	DefaultSubtitleStreamIndex *int                `json:"DefaultSubtitleStreamIndex,omitempty"`
	RawMediaStreamsJSON        json.RawMessage     `json:"-"`
	RawJSON                    json.RawMessage     `json:"-"`
}

type remoteMediaSourceAlias RemoteMediaSource

// UnmarshalJSON decodes RemoteMediaSource while retaining full raw JSON and raw MediaStreams JSON.
func (m *RemoteMediaSource) UnmarshalJSON(data []byte) error {
	var alias remoteMediaSourceAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*m = RemoteMediaSource(alias)
	m.RawJSON = append(json.RawMessage(nil), data...)

	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawMap); err == nil {
		if streamsRaw, ok := rawMap["MediaStreams"]; ok && len(streamsRaw) > 0 && string(streamsRaw) != "null" {
			m.RawMediaStreamsJSON = append(json.RawMessage(nil), streamsRaw...)
		}
	}
	return nil
}

// RemoteUserData represents user playback/favorite state on an upstream Jellyfin item.
type RemoteUserData struct {
	PlaybackPositionTicks int64  `json:"PlaybackPositionTicks"`
	PlayCount             int    `json:"PlayCount"`
	IsFavorite            bool   `json:"IsFavorite"`
	Played                bool   `json:"Played"`
	LastPlayedDate        string `json:"LastPlayedDate,omitempty"`
	Key                   string `json:"Key,omitempty"`
}

// RemoteItem represents a Jellyfin BaseItemDto returned by upstream item queries.
type RemoteItem struct {
	ID                string              `json:"Id"`
	Name              string              `json:"Name"`
	OriginalTitle     string              `json:"OriginalTitle,omitempty"`
	SortName          string              `json:"SortName,omitempty"`
	Overview          string              `json:"Overview,omitempty"`
	Taglines          []string            `json:"Taglines,omitempty"`
	Type              string              `json:"Type"`
	ParentID          string              `json:"ParentId,omitempty"`
	SeriesID          string              `json:"SeriesId,omitempty"`
	SeriesName        string              `json:"SeriesName,omitempty"`
	SeasonID          string              `json:"SeasonId,omitempty"`
	SeasonName        string              `json:"SeasonName,omitempty"`
	IndexNumber       int                 `json:"IndexNumber,omitempty"`
	ParentIndexNumber int                 `json:"ParentIndexNumber,omitempty"`
	ProductionYear    int                 `json:"ProductionYear,omitempty"`
	PremiereDate      string              `json:"PremiereDate,omitempty"`
	DateCreated       string              `json:"DateCreated,omitempty"`
	CommunityRating   float64             `json:"CommunityRating,omitempty"`
	OfficialRating    string              `json:"OfficialRating,omitempty"`
	RunTimeTicks      int64               `json:"RunTimeTicks,omitempty"`
	Genres            []string            `json:"Genres,omitempty"`
	Studios           []NameID            `json:"Studios,omitempty"`
	People            []PersonInfo        `json:"People,omitempty"`
	ProviderIDs       map[string]string   `json:"ProviderIds,omitempty"`
	ImageTags         map[string]string   `json:"ImageTags,omitempty"`
	BackdropImageTags []string            `json:"BackdropImageTags,omitempty"`
	MediaSources      []RemoteMediaSource `json:"MediaSources,omitempty"`
	UserData          *RemoteUserData     `json:"UserData,omitempty"`
}

type itemsPageResponse struct {
	Items            []RemoteItem `json:"Items"`
	TotalRecordCount int          `json:"TotalRecordCount"`
}

func authHeaderForServer(accessToken string) string {
	return fmt.Sprintf(`%s, Token=%q`, jellyfinAuthHeader, accessToken)
}

func (c *Client) doRequest(ctx context.Context, method, fullURL, accessToken string, body []byte, contentType string) ([]byte, http.Header, int, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", authHeaderForServer(accessToken))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.Header, resp.StatusCode, err
	}
	return respBody, resp.Header, resp.StatusCode, nil
}

// GetItemsPage fetches a single page of items of the requested types from an upstream Jellyfin server.
func (c *Client) GetItemsPage(ctx context.Context, srv db.UpstreamServer, itemTypes []string, startIndex, limit int) ([]RemoteItem, int, error) {
	base := strings.TrimRight(strings.TrimSpace(srv.BaseURL), "/")
	if base == "" {
		return nil, 0, fmt.Errorf("upstream server %q has empty base URL", srv.ID)
	}
	if limit <= 0 {
		limit = defaultPageSize
	}

	q := url.Values{}
	q.Set("Recursive", "true")
	if len(itemTypes) > 0 {
		q.Set("IncludeItemTypes", strings.Join(itemTypes, ","))
	}
	q.Set("Fields", itemQueryFields)
	q.Set("StartIndex", strconv.Itoa(startIndex))
	q.Set("Limit", strconv.Itoa(limit))

	path := "/Items"
	if srv.RemoteUserID != "" {
		path = "/Users/" + url.PathEscape(srv.RemoteUserID) + "/Items"
	}
	fullURL := base + path + "?" + q.Encode()

	respBody, _, status, err := c.doRequest(ctx, http.MethodGet, fullURL, srv.AccessToken, nil, "")
	if err != nil {
		return nil, 0, fmt.Errorf("request items page: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, 0, fmt.Errorf("upstream %s returned HTTP %d: %s", fullURL, status, strings.TrimSpace(string(respBody)))
	}

	var parsed itemsPageResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, 0, fmt.Errorf("decode items page: %w", err)
	}
	return parsed.Items, parsed.TotalRecordCount, nil
}

// FetchAllItems paginates through all upstream items of the given types using page size 200.
func (c *Client) FetchAllItems(ctx context.Context, srv db.UpstreamServer, itemTypes []string) ([]RemoteItem, error) {
	var all []RemoteItem
	startIndex := 0
	for {
		items, total, err := c.GetItemsPage(ctx, srv, itemTypes, startIndex, defaultPageSize)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		all = append(all, items...)
		startIndex += len(items)
		if total > 0 && len(all) >= total {
			break
		}
		if total == 0 && len(items) < defaultPageSize {
			break
		}
	}
	return all, nil
}

// FetchImage retrieves an image from an upstream Jellyfin server, forwarding supported image query params.
func (c *Client) FetchImage(ctx context.Context, srv db.UpstreamServer, remoteItemID, imageType string, query url.Values) ([]byte, string, int, error) {
	base := strings.TrimRight(strings.TrimSpace(srv.BaseURL), "/")
	if base == "" {
		return nil, "", 0, fmt.Errorf("upstream server %q has empty base URL", srv.ID)
	}

	fwd := url.Values{}
	if query != nil {
		allowed := map[string]string{
			"maxwidth":  "maxWidth",
			"maxheight": "maxHeight",
			"quality":   "quality",
			"tag":       "tag",
			"format":    "format",
		}
		for k, vals := range query {
			canon, ok := allowed[strings.ToLower(k)]
			if !ok {
				continue
			}
			for _, v := range vals {
				if v != "" {
					fwd.Add(canon, v)
				}
			}
		}
	}

	fullURL := base + "/Items/" + url.PathEscape(remoteItemID) + "/Images/" + url.PathEscape(imageType)
	if encoded := fwd.Encode(); encoded != "" {
		fullURL += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("Authorization", authHeaderForServer(srv.AccessToken))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.Header.Get("Content-Type"), resp.StatusCode, err
	}
	return data, resp.Header.Get("Content-Type"), resp.StatusCode, nil
}

// PostPlaybackInfo proxies a PlaybackInfo negotiation request to an upstream Jellyfin server.
func (c *Client) PostPlaybackInfo(ctx context.Context, srv db.UpstreamServer, remoteItemID string, query url.Values, body []byte) ([]byte, int, error) {
	base := strings.TrimRight(strings.TrimSpace(srv.BaseURL), "/")
	if base == "" {
		return nil, 0, fmt.Errorf("upstream server %q has empty base URL", srv.ID)
	}

	fullURL := base + "/Items/" + url.PathEscape(remoteItemID) + "/PlaybackInfo"
	if query != nil {
		if encoded := query.Encode(); encoded != "" {
			fullURL += "?" + encoded
		}
	}

	respBody, _, status, err := c.doRequest(ctx, http.MethodPost, fullURL, srv.AccessToken, body, "application/json")
	if err != nil {
		return nil, 0, err
	}
	return respBody, status, nil
}

// ReportPlaying notifies the upstream server that playback started (/Sessions/Playing).
func (c *Client) ReportPlaying(ctx context.Context, srv db.UpstreamServer, body []byte) error {
	return c.postSimple(ctx, srv, "/Sessions/Playing", body)
}

// ReportProgress notifies the upstream server of playback progress (/Sessions/Playing/Progress).
func (c *Client) ReportProgress(ctx context.Context, srv db.UpstreamServer, body []byte) error {
	return c.postSimple(ctx, srv, "/Sessions/Playing/Progress", body)
}

// ReportStopped notifies the upstream server that playback stopped (/Sessions/Playing/Stopped).
func (c *Client) ReportStopped(ctx context.Context, srv db.UpstreamServer, body []byte) error {
	return c.postSimple(ctx, srv, "/Sessions/Playing/Stopped", body)
}

func (c *Client) postSimple(ctx context.Context, srv db.UpstreamServer, path string, body []byte) error {
	base := strings.TrimRight(strings.TrimSpace(srv.BaseURL), "/")
	if base == "" {
		return fmt.Errorf("upstream server %q has empty base URL", srv.ID)
	}
	fullURL := base + path
	respBody, _, status, err := c.doRequest(ctx, http.MethodPost, fullURL, srv.AccessToken, body, "application/json")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("upstream %s returned HTTP %d: %s", path, status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// MarkPlayed marks or unmarks an item as played on the upstream Jellyfin server.
func (c *Client) MarkPlayed(ctx context.Context, srv db.UpstreamServer, remoteItemID string, played bool) error {
	base := strings.TrimRight(strings.TrimSpace(srv.BaseURL), "/")
	if base == "" {
		return fmt.Errorf("upstream server %q has empty base URL", srv.ID)
	}
	method := http.MethodDelete
	if played {
		method = http.MethodPost
	}
	fullURL := base + "/Users/" + url.PathEscape(srv.RemoteUserID) + "/PlayedItems/" + url.PathEscape(remoteItemID)
	respBody, _, status, err := c.doRequest(ctx, method, fullURL, srv.AccessToken, nil, "")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("upstream mark played returned HTTP %d: %s", status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// MarkFavorite marks or unmarks an item as favorite on the upstream Jellyfin server.
func (c *Client) MarkFavorite(ctx context.Context, srv db.UpstreamServer, remoteItemID string, favorite bool) error {
	base := strings.TrimRight(strings.TrimSpace(srv.BaseURL), "/")
	if base == "" {
		return fmt.Errorf("upstream server %q has empty base URL", srv.ID)
	}
	method := http.MethodDelete
	if favorite {
		method = http.MethodPost
	}
	fullURL := base + "/Users/" + url.PathEscape(srv.RemoteUserID) + "/FavoriteItems/" + url.PathEscape(remoteItemID)
	respBody, _, status, err := c.doRequest(ctx, method, fullURL, srv.AccessToken, nil, "")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("upstream mark favorite returned HTTP %d: %s", status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// StopActiveEncodings stops active transcodes on the upstream Jellyfin server.
func (c *Client) StopActiveEncodings(ctx context.Context, srv db.UpstreamServer, deviceID, playSessionID string) error {
	base := strings.TrimRight(strings.TrimSpace(srv.BaseURL), "/")
	if base == "" {
		return fmt.Errorf("upstream server %q has empty base URL", srv.ID)
	}
	q := url.Values{}
	if deviceID != "" {
		q.Set("deviceId", deviceID)
	}
	if playSessionID != "" {
		q.Set("playSessionId", playSessionID)
	}
	fullURL := base + "/Videos/ActiveEncodings"
	if encoded := q.Encode(); encoded != "" {
		fullURL += "?" + encoded
	}
	respBody, _, status, err := c.doRequest(ctx, http.MethodDelete, fullURL, srv.AccessToken, nil, "")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("upstream stop active encodings returned HTTP %d: %s", status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// GetUserItemData fetches user playback/favorite state for a single item on the upstream server.
func (c *Client) GetUserItemData(ctx context.Context, srv db.UpstreamServer, remoteItemID string) (*RemoteUserData, error) {
	base := strings.TrimRight(strings.TrimSpace(srv.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("upstream server %q has empty base URL", srv.ID)
	}
	path := "/Items/" + url.PathEscape(remoteItemID)
	if srv.RemoteUserID != "" {
		path = "/Users/" + url.PathEscape(srv.RemoteUserID) + path
	}
	fullURL := base + path

	respBody, _, status, err := c.doRequest(ctx, http.MethodGet, fullURL, srv.AccessToken, nil, "")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("upstream get item user data returned HTTP %d: %s", status, strings.TrimSpace(string(respBody)))
	}
	var item RemoteItem
	if err := json.Unmarshal(respBody, &item); err != nil {
		return nil, fmt.Errorf("decode item user data: %w", err)
	}
	if item.UserData == nil {
		return &RemoteUserData{}, nil
	}
	return item.UserData, nil
}
