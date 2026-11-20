package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/tsrz/mergerfin/internal/db"
	"github.com/tsrz/mergerfin/internal/idgen"
)

// QueryResult is the standard Jellyfin paged query envelope.
type QueryResult struct {
	Items            []BaseItemDto `json:"Items"`
	TotalRecordCount int           `json:"TotalRecordCount"`
	StartIndex       int           `json:"StartIndex"`
}

// NameIDDto represents a Jellyfin Name/Id pair (used in Studios and GenreItems).
type NameIDDto struct {
	Name string `json:"Name"`
	Id   string `json:"Id,omitempty"`
}

// PersonDto represents a cast/crew entry in a BaseItemDto.
type PersonDto struct {
	Name            string `json:"Name"`
	Id              string `json:"Id,omitempty"`
	Role            string `json:"Role,omitempty"`
	Type            string `json:"Type,omitempty"`
	PrimaryImageTag string `json:"PrimaryImageTag,omitempty"`
}

// MediaSourceDto describes a playable source attached to a virtual Movie or Episode.
type MediaSourceDto struct {
	Protocol                   string            `json:"Protocol"`
	Id                         string            `json:"Id"`
	Path                       string            `json:"Path,omitempty"`
	Type                       string            `json:"Type"`
	Container                  string            `json:"Container,omitempty"`
	Size                       int64             `json:"Size,omitempty"`
	Name                       string            `json:"Name"`
	IsRemote                   bool              `json:"IsRemote"`
	ETag                       string            `json:"ETag,omitempty"`
	RunTimeTicks               int64             `json:"RunTimeTicks,omitempty"`
	ReadAtNativeFramerate      bool              `json:"ReadAtNativeFramerate"`
	IgnoreDts                  bool              `json:"IgnoreDts"`
	IgnoreIndex                bool              `json:"IgnoreIndex"`
	GenPtsInput                bool              `json:"GenPtsInput"`
	SupportsTranscoding        bool              `json:"SupportsTranscoding"`
	SupportsDirectStream       bool              `json:"SupportsDirectStream"`
	SupportsDirectPlay         bool              `json:"SupportsDirectPlay"`
	IsInfiniteStream           bool              `json:"IsInfiniteStream"`
	RequiresOpening            bool              `json:"RequiresOpening"`
	RequiresClosing            bool              `json:"RequiresClosing"`
	SupportsProbing            bool              `json:"SupportsProbing"`
	VideoType                  string            `json:"VideoType,omitempty"`
	MediaStreams               []any             `json:"MediaStreams"`
	MediaAttachments           []any             `json:"MediaAttachments"`
	Formats                    []any             `json:"Formats"`
	Bitrate                    int64             `json:"Bitrate,omitempty"`
	RequiredHttpHeaders        map[string]string `json:"RequiredHttpHeaders"`
	DefaultAudioStreamIndex    *int              `json:"DefaultAudioStreamIndex,omitempty"`
	DefaultSubtitleStreamIndex *int              `json:"DefaultSubtitleStreamIndex,omitempty"`
}

// UserItemDataDto represents per-user playback and favorite state on a BaseItemDto.
type UserItemDataDto struct {
	UnplayedItemCount     *int   `json:"UnplayedItemCount,omitempty"`
	PlaybackPositionTicks int64  `json:"PlaybackPositionTicks"`
	PlayCount             int    `json:"PlayCount"`
	IsFavorite            bool   `json:"IsFavorite"`
	Played                bool   `json:"Played"`
	Key                   string `json:"Key"`
	LastPlayedDate        string `json:"LastPlayedDate,omitempty"`
}

// BaseItemDto mirrors the Jellyfin 12.4 item payload with strict 32-hex IDs and non-null slices/maps.
type BaseItemDto struct {
	Name                    string            `json:"Name"`
	OriginalTitle           string            `json:"OriginalTitle,omitempty"`
	ServerId                string            `json:"ServerId"`
	Id                      string            `json:"Id"`
	Etag                    string            `json:"Etag"`
	DateCreated             string            `json:"DateCreated,omitempty"`
	CanDelete               bool              `json:"CanDelete"`
	CanDownload             bool              `json:"CanDownload"`
	SortName                string            `json:"SortName,omitempty"`
	PremiereDate            string            `json:"PremiereDate,omitempty"`
	ExternalUrls            []any             `json:"ExternalUrls"`
	Overview                string            `json:"Overview,omitempty"`
	Taglines                []string          `json:"Taglines"`
	Genres                  []string          `json:"Genres"`
	GenreItems              []NameIDDto       `json:"GenreItems"`
	Studios                 []NameIDDto       `json:"Studios"`
	People                  []PersonDto       `json:"People"`
	ProductionYear          int               `json:"ProductionYear,omitempty"`
	OfficialRating          string            `json:"OfficialRating,omitempty"`
	CommunityRating         float64           `json:"CommunityRating,omitempty"`
	RunTimeTicks            int64             `json:"RunTimeTicks,omitempty"`
	PlayAccess              string            `json:"PlayAccess"`
	IndexNumber             *int              `json:"IndexNumber,omitempty"`
	ParentIndexNumber       *int              `json:"ParentIndexNumber,omitempty"`
	ParentId                string            `json:"ParentId,omitempty"`
	SeriesId                string            `json:"SeriesId,omitempty"`
	SeriesName              string            `json:"SeriesName,omitempty"`
	SeasonId                string            `json:"SeasonId,omitempty"`
	SeasonName              string            `json:"SeasonName,omitempty"`
	SeriesPrimaryImageTag   string            `json:"SeriesPrimaryImageTag,omitempty"`
	ParentBackdropItemId    string            `json:"ParentBackdropItemId,omitempty"`
	ParentBackdropImageTags []string          `json:"ParentBackdropImageTags,omitempty"`
	IsFolder                bool              `json:"IsFolder"`
	Type                    string            `json:"Type"`
	CollectionType          string            `json:"CollectionType,omitempty"`
	MediaType               string            `json:"MediaType,omitempty"`
	LocationType            string            `json:"LocationType"`
	ImageTags               map[string]string `json:"ImageTags"`
	BackdropImageTags       []string          `json:"BackdropImageTags"`
	ProviderIds             map[string]string `json:"ProviderIds"`
	RemoteTrailers          []any             `json:"RemoteTrailers"`
	LockedFields            []string          `json:"LockedFields"`
	Tags                    []string          `json:"Tags"`
	UserData                UserItemDataDto   `json:"UserData"`
	MediaSources            []MediaSourceDto  `json:"MediaSources,omitempty"`
}

func ensureHex32(id string) string {
	if norm, err := idgen.NormalizeID(id); err == nil {
		return norm
	}
	return id
}

func libraryToDTO(ctx context.Context, database *db.DB, serverID, userID string, lib db.VirtualLibrary) BaseItemDto {
	libID := ensureHex32(lib.ID)
	var unplayedPtr *int
	if database != nil && userID != "" {
		if cnt, err := database.CountUnplayedInSubtree(ctx, userID, libID); err == nil {
			unplayedPtr = &cnt
		}
	}

	return BaseItemDto{
		Name:              lib.Name,
		ServerId:          ensureHex32(serverID),
		Id:                libID,
		Etag:              libID,
		PlayAccess:        "Full",
		IsFolder:          true,
		Type:              "CollectionFolder",
		CollectionType:    lib.CollectionType,
		LocationType:      "FileSystem",
		ExternalUrls:      []any{},
		Taglines:          []string{},
		Genres:            []string{},
		GenreItems:        []NameIDDto{},
		Studios:           []NameIDDto{},
		People:            []PersonDto{},
		ImageTags:         map[string]string{},
		BackdropImageTags: []string{},
		ProviderIds:       map[string]string{},
		RemoteTrailers:    []any{},
		LockedFields:      []string{},
		Tags:              []string{},
		UserData: UserItemDataDto{
			UnplayedItemCount: unplayedPtr,
			Key:               libID,
		},
	}
}

func buildItemDTOs(ctx context.Context, database *db.DB, serverID, userID string, items []db.VirtualItem, includeMediaSources bool) ([]BaseItemDto, error) {
	dtos := make([]BaseItemDto, 0, len(items))
	if len(items) == 0 {
		return dtos, nil
	}

	itemIDs := make([]string, len(items))
	for i, it := range items {
		itemIDs[i] = it.ID
	}

	var userDataMap map[string]db.UserItemData
	if database != nil && userID != "" {
		var err error
		userDataMap, err = database.BatchUserItemData(ctx, userID, itemIDs)
		if err != nil {
			return nil, err
		}
	}

	var sourcesMap map[string][]db.ItemSource
	if includeMediaSources && database != nil {
		var err error
		sourcesMap, err = database.ListSourcesByItemIDs(ctx, itemIDs)
		if err != nil {
			return nil, err
		}
	}

	normServerID := ensureHex32(serverID)

	for _, it := range items {
		itemID := ensureHex32(it.ID)
		isFolder := it.ItemType == "Series" || it.ItemType == "Season" || it.ItemType == "CollectionFolder" || it.ItemType == "Folder" || it.ItemType == "BoxSet"

		mediaType := ""
		if it.ItemType == "Movie" || it.ItemType == "Episode" {
			mediaType = "Video"
		}

		parentID := ensureHex32(it.ParentID)
		if parentID == "" {
			if it.ItemType == "Episode" && it.SeasonID != "" {
				parentID = ensureHex32(it.SeasonID)
			} else if it.ItemType == "Season" && it.SeriesID != "" {
				parentID = ensureHex32(it.SeriesID)
			} else if it.LibraryID != "" {
				parentID = ensureHex32(it.LibraryID)
			}
		}

		genres := parseStringSlice(it.GenresJSON)
		genreItems := make([]NameIDDto, 0, len(genres))
		for _, g := range genres {
			genreItems = append(genreItems, NameIDDto{
				Name: g,
				Id:   idgen.DeterministicHex32("genre:" + strings.ToLower(g)),
			})
		}

		studios := parseStudios(it.StudiosJSON)
		people := parsePeople(it.PeopleJSON)
		providerIDs := parseStringMap(it.ProviderIDsJSON)

		taglines := []string{}
		if strings.TrimSpace(it.Tagline) != "" {
			taglines = []string{it.Tagline}
		}

		imageTags := map[string]string{}
		if it.ImgPrimaryServerID != "" && it.ImgPrimaryRemoteID != "" {
			imageTags["Primary"] = idgen.DeterministicHex32("img:primary:" + it.ImgPrimaryServerID + ":" + it.ImgPrimaryRemoteID)
		}

		backdropTags := []string{}
		if it.ImgBackdropServerID != "" && it.ImgBackdropRemoteID != "" {
			backdropTags = []string{idgen.DeterministicHex32("img:backdrop:" + it.ImgBackdropServerID + ":" + it.ImgBackdropRemoteID)}
		}

		ud := userDataMap[it.ID]
		userDataDTO := UserItemDataDto{
			PlaybackPositionTicks: ud.PlaybackPositionTicks,
			PlayCount:             ud.PlayCount,
			IsFavorite:            ud.IsFavorite,
			Played:                ud.Played,
			Key:                   itemID,
			LastPlayedDate:        ud.LastPlayedAt,
		}
		if (it.ItemType == "Series" || it.ItemType == "Season") && database != nil && userID != "" {
			if unplayed, err := database.CountUnplayedInSubtree(ctx, userID, itemID); err == nil {
				userDataDTO.UnplayedItemCount = &unplayed
			}
		}

		var idxNum, parentIdxNum *int
		if it.ItemType == "Episode" || it.ItemType == "Season" || it.IndexNumber > 0 {
			v := it.IndexNumber
			idxNum = &v
		}
		if it.ItemType == "Episode" || it.ParentIndexNumber > 0 {
			v := it.ParentIndexNumber
			parentIdxNum = &v
		}

		dto := BaseItemDto{
			Name:              it.Name,
			OriginalTitle:     it.OriginalTitle,
			ServerId:          normServerID,
			Id:                itemID,
			Etag:              itemID,
			DateCreated:       it.DateCreated,
			CanDelete:         false,
			CanDownload:       !isFolder,
			SortName:          it.SortName,
			PremiereDate:      it.PremiereDate,
			ExternalUrls:      []any{},
			Overview:          it.Overview,
			Taglines:          taglines,
			Genres:            genres,
			GenreItems:        genreItems,
			Studios:           studios,
			People:            people,
			ProductionYear:    it.ProductionYear,
			OfficialRating:    it.OfficialRating,
			CommunityRating:   it.CommunityRating,
			RunTimeTicks:      it.RunTimeTicks,
			PlayAccess:        "Full",
			IndexNumber:       idxNum,
			ParentIndexNumber: parentIdxNum,
			ParentId:          parentID,
			SeriesId:          ensureHex32(it.SeriesID),
			SeriesName:        it.SeriesName,
			SeasonId:          ensureHex32(it.SeasonID),
			SeasonName:        it.SeasonName,
			IsFolder:          isFolder,
			Type:              it.ItemType,
			MediaType:         mediaType,
			LocationType:      "FileSystem",
			ImageTags:         imageTags,
			BackdropImageTags: backdropTags,
			ProviderIds:       providerIDs,
			RemoteTrailers:    []any{},
			LockedFields:      []string{},
			Tags:              []string{},
			UserData:          userDataDTO,
		}

		if includeMediaSources && !isFolder {
			rawSources := sourcesMap[it.ID]
			dto.MediaSources = make([]MediaSourceDto, 0, len(rawSources))
			for _, src := range rawSources {
				dto.MediaSources = append(dto.MediaSources, toMediaSourceDto(src))
			}
		}

		dtos = append(dtos, dto)
	}

	return dtos, nil
}

func toMediaSourceDto(src db.ItemSource) MediaSourceDto {
	sourceID := ensureHex32(src.SourceID)
	streams := parseAnySlice(src.RawMediaStreamsJSON)

	ms := MediaSourceDto{
		Protocol:            "File",
		Id:                  sourceID,
		Type:                "Default",
		Container:           src.Container,
		Size:                src.SizeBytes,
		Name:                src.Label,
		ETag:                sourceID,
		RunTimeTicks:        src.RunTimeTicks,
		SupportsTranscoding: true,
		SupportsDirectStream: true,
		SupportsDirectPlay:  true,
		SupportsProbing:     true,
		MediaStreams:        streams,
		MediaAttachments:    []any{},
		Formats:             []any{},
		Bitrate:             src.Bitrate,
		RequiredHttpHeaders: map[string]string{},
	}

	if strings.TrimSpace(src.RawMediaSourceJSON) != "" && src.RawMediaSourceJSON != "{}" {
		var raw struct {
			Path                       string `json:"Path"`
			Protocol                   string `json:"Protocol"`
			VideoType                  string `json:"VideoType"`
			DefaultAudioStreamIndex    *int   `json:"DefaultAudioStreamIndex"`
			DefaultSubtitleStreamIndex *int   `json:"DefaultSubtitleStreamIndex"`
		}
		if err := json.Unmarshal([]byte(src.RawMediaSourceJSON), &raw); err == nil {
			ms.Path = raw.Path
			if raw.Protocol != "" {
				ms.Protocol = raw.Protocol
			}
			ms.VideoType = raw.VideoType
			ms.DefaultAudioStreamIndex = raw.DefaultAudioStreamIndex
			ms.DefaultSubtitleStreamIndex = raw.DefaultSubtitleStreamIndex
		}
	}

	return ms
}

func parseStringSlice(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func parseStudios(raw string) []NameIDDto {
	if strings.TrimSpace(raw) == "" {
		return []NameIDDto{}
	}
	var out []NameIDDto
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []NameIDDto{}
	}
	for i := range out {
		if out[i].Id != "" {
			out[i].Id = ensureHex32(out[i].Id)
		}
	}
	return out
}

func parsePeople(raw string) []PersonDto {
	if strings.TrimSpace(raw) == "" {
		return []PersonDto{}
	}
	var out []PersonDto
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []PersonDto{}
	}
	for i := range out {
		if out[i].Id != "" {
			out[i].Id = ensureHex32(out[i].Id)
		}
	}
	return out
}

func parseStringMap(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return map[string]string{}
	}
	return out
}

func parseAnySlice(raw string) []any {
	if strings.TrimSpace(raw) == "" {
		return []any{}
	}
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []any{}
	}
	return out
}
