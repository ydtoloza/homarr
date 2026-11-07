// Package emby is the Go port of the upstream EmbyIntegration
// (packages/integrations/src/emby/emby-integration.ts), restricted to the
// media-server surface the widget consumes: getCurrentSessionsAsync
// (GET {url}/emby/Sessions) plus the framework Check (testingAsync: GET
// {url}/emby/System/Ping, emby-integration.ts:97-113). The
// IMediaReleasesIntegration surface of the same class is NOT ported here.
//
// Request semantics pinned from emby-integration.ts:
//   - auth: EVERY call carries BOTH headers (:93-95, :116-122)
//     `X-Emby-Token: <apiKey secret>` and
//     `Authorization: Emby Client="Dashboard", Device="Homarr",
//     DeviceId="homarr-emby-integration", Version="0.0.1"`
//     (integrationDefs emby secretKinds [["apiKey"]], httpAuth
//     {type: "apiKeyHeader", name: "X-Emby-Token"}),
//   - sessions (:115-222): a non-2xx answer throws the plain Error
//     `Emby server <id> returned a non successful status code: <status>` and
//     a zod mismatch `Emby server <id> returned an unexpected response: ...`
//     (:124-132) — both plain (unknown taxonomy; their message never reaches
//     the wire since the widget query only reports the public constant),
//   - filters (:134-137): UserId must be present, DeviceId !== the homarr
//     device id "homarr-emby-integration", showOnlyPlaying requires
//     NowPlayingItem,
//   - the session mapping reuses the jellyfin module helpers verbatim
//     (import { convertJellyfinType, parseLocation, ticksToMs } :13) — the
//     Go port calls the exported jellyfin.ConvertType/ParseLocation/TicksToMs,
//   - NO per-request timeout upstream (fetchWithTrustedCertificatesAsync
//     without options); the widget layer deadline (60s) applies.
package emby

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver/jellyfin"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "emby").
const Kind = "emby"

// apiKeyHeader / deviceID / authorizationHeaderValue mirror the statics of
// EmbyIntegration (:93-95).
const (
	apiKeyHeader             = "X-Emby-Token"
	deviceID                 = "homarr-emby-integration"
	authorizationHeaderValue = `Emby Client="Dashboard", Device="Homarr", DeviceId="` + deviceID + `", Version="0.0.1"`
)

// Definition returns the metadata of the emby kind.
func Definition() integrations.Definition { return integrations.Definition{Kind: Kind, Name: "Emby"} }

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values emby consumes
// (secretKinds [["apiKey"]] — required).
type Secrets struct {
	APIKey string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map.
func SecretsFromMap(m map[string]string) (Secrets, error) {
	apiKey, err := mediaserver.SecretValue(m, "apiKey")
	return Secrets{APIKey: apiKey}, err
}

// Integration is one Emby instance. All methods are safe for concurrent use.
type Integration struct {
	input   integrations.Input
	secrets Secrets
	urls    mediaorganizer.URLBuilder
	http    mediaorganizer.HTTPClient
}

// New builds one Emby client. An empty apiKey is reported at call time as
// the upstream missing-secret error.
func New(input integrations.Input, secrets Secrets) *Integration {
	return &Integration{
		input:   input,
		secrets: secrets,
		urls:    mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
	}
}

// NewFromRef builds the client from a widget-router IntegrationRef.
func NewFromRef(ref mediaserver.IntegrationRef) (*Integration, error) {
	secrets, err := SecretsFromMap(ref.Secrets)
	if err != nil {
		return nil, err
	}
	return New(ref.Input(), secrets), nil
}

func (i *Integration) headers() (http.Header, error) {
	if i.secrets.APIKey == "" {
		return nil, &mediaorganizer.MissingSecretError{Kind: "apiKey"}
	}
	return http.Header{
		apiKeyHeader:    []string{i.secrets.APIKey},
		"Authorization": []string{authorizationHeaderValue},
	}, nil
}

// Check implements integrations.Integration: GET {url}/emby/System/Ping with
// both auth headers (testingAsync, emby-integration.ts:97-113).
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	header, err := i.headers()
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	url, err := i.urls.Build("/emby/System/Ping", nil)
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	res, err := i.http.Do(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		return integrations.CheckResult{Error: mediaserver.MsgRequest}, nil
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return integrations.CheckResult{
			StatusCode: res.StatusCode,
			Error:      mediaserver.MsgResponse,
		}, nil
	}
	return integrations.CheckResult{StatusCode: res.StatusCode}, nil
}

// GetCurrentSessions ports getCurrentSessionsAsync (emby-integration.ts:115-222).
func (i *Integration) GetCurrentSessions(ctx context.Context, input mediaserver.CurrentSessionsInput) ([]mediaserver.StreamSession, error) {
	header, err := i.headers()
	if err != nil {
		return nil, err
	}
	url, err := i.urls.Build("/emby/Sessions", nil)
	if err != nil {
		return nil, err
	}
	res, err := i.http.Do(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		return nil, mediaserver.NewRequestError(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		// plain Error upstream — the message is the contract
		return nil, fmt.Errorf("Emby server %s returned an unexpected response: %w", i.input.ID, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// plain Error upstream (message pinned :124-125)
		return nil, fmt.Errorf("Emby server %s returned a non successful status code: %d", i.input.ID, res.StatusCode)
	}
	var sessions []sessionDTO
	if err := json.Unmarshal(raw, &sessions); err != nil {
		// z.array(sessionSchema).safeParse failure (:128-132)
		return nil, fmt.Errorf("Emby server %s returned an unexpected response: %v", i.input.ID, err)
	}

	out := make([]mediaserver.StreamSession, 0, len(sessions))
	for idx := range sessions {
		info := &sessions[idx]
		// .filter(sessionInfo => sessionInfo.UserId !== undefined)
		if info.UserID == nil {
			continue
		}
		// .filter(sessionInfo => sessionInfo.DeviceId !== "homarr-emby-integration")
		if info.DeviceID != nil && *info.DeviceID == deviceID {
			continue
		}
		if input.ShowOnlyPlaying && info.NowPlayingItem == nil {
			continue
		}
		out = append(out, i.sessionOf(info))
	}
	return out, nil
}

// sessionOf maps one /Sessions entry (:138-221) — the same shape as the
// jellyfin mapper with the emby URL style for the profile picture.
func (i *Integration) sessionOf(info *sessionDTO) mediaserver.StreamSession {
	var currentlyPlaying *mediaserver.CurrentlyPlaying
	if info.NowPlayingItem != nil {
		currentlyPlaying = i.currentlyPlayingOf(info.NowPlayingItem, info.PlayState, info.TranscodingInfo, deref(info.RemoteEndPoint))
	}

	userID := deref(info.UserID)
	profilePictureURL, _ := i.urls.BuildExternal("/Users/"+userID+"/Images/Primary", nil)

	return mediaserver.StreamSession{
		SessionID:   deref(info.ID),
		SessionName: deref(info.Client) + " (" + deref(info.DeviceName) + ")",
		User: mediaserver.SessionUser{
			UserID:            userID,
			Username:          deref(info.UserName),
			ProfilePictureURL: mediaserver.Ptr(profilePictureURL),
		},
		CurrentlyPlaying: currentlyPlaying,
	}
}

// currentlyPlayingOf mirrors emby-integration.ts:141-209 — identical to the
// jellyfin mapping except that every source is the zod-validated session
// (nullish members) and the helpers come from the jellyfin module.
func (i *Integration) currentlyPlayingOf(np *nowPlayingDTO, ps *playStateDTO, tr *transcodingDTO, remoteEndPoint string) *mediaserver.CurrentlyPlaying {
	isEpisode := np.Type == "Episode"

	var state string
	if ps != nil && ps.IsPaused != nil && *ps.IsPaused {
		state = mediaserver.StatePaused
	} else {
		state = mediaserver.StatePlaying
	}

	containerChanged := ps != nil && ps.PlayMethod != nil && *ps.PlayMethod == "DirectStream"

	transcoding := mediaserver.Transcoding{
		Target:        mediaserver.Target{},
		IsVideoDirect: true,
		IsAudioDirect: true,
	}
	var frameRate, audioChannels *float64
	var audioCodec *string
	if tr != nil {
		directVideo := true
		if tr.IsVideoDirect != nil {
			directVideo = *tr.IsVideoDirect
		}
		directAudio := true
		if tr.IsAudioDirect != nil {
			directAudio = *tr.IsAudioDirect
		}
		transcoding = mediaserver.Transcoding{
			Container:        tr.Container,
			Resolution:       truthyResolution(tr.Width, tr.Height),
			Target:           mediaserver.Target{AudioCodec: tr.AudioCodec, VideoCodec: tr.VideoCodec},
			IsVideoDirect:    directVideo,
			IsAudioDirect:    directAudio,
			ContainerChanged: containerChanged,
		}
		frameRate = tr.Framerate
		audioChannels = tr.AudioChannels
		audioCodec = tr.AudioCodec
	}

	// bitrate fallback chain (:146-157) — identical to jellyfin.
	sum := 0.0
	for _, stream := range np.MediaStreams {
		if stream.BitRate != nil {
			sum += *stream.BitRate
		}
	}
	var bitrateBps *float64
	switch {
	case tr != nil && tr.Bitrate != nil:
		bitrateBps = tr.Bitrate
	case len(np.MediaSources) > 0 && np.MediaSources[0].Bitrate != nil:
		bitrateBps = np.MediaSources[0].Bitrate
	case sum != 0:
		bitrateBps = &sum
	}
	var bitrateKbps *float64
	if bitrateBps != nil {
		bitrateKbps = mediaserver.Ptr(math.Round(*bitrateBps / 1000))
	}

	return &mediaserver.CurrentlyPlaying{
		Type:          jellyfin.ConvertType(np.Type),
		Name:          coalesce(np.SeriesName, np.Name),
		SeasonName:    mediaserver.Ptr(coalesce(np.SeasonName)),
		SeasonNumber:  episodeOr(isEpisode, np.ParentIndexNumber),
		EpisodeName:   mediaserver.OptionalNullish(episodeNameRaw(isEpisode, np.EpisodeTitle)),
		EpisodeNumber: episodeOr(isEpisode, np.IndexNumber),
		AlbumName:     mediaserver.Ptr(coalesce(np.Album)),
		EpisodeCount:  mediaserver.OptionalNullish(np.EpisodeCount),
		Playback: mediaserver.Playback{
			State:      mediaserver.Ptr(state),
			PositionMs: jellyfin.TicksToMs(playStateTicks(ps)),
			DurationMs: jellyfin.TicksToMs(np.RunTimeTicks),
		},
		Location: jellyfin.ParseLocation(remoteEndPoint),
		Metadata: &mediaserver.Metadata{
			Video: mediaserver.MetadataVideo{
				Resolution: truthyResolution(np.Width, np.Height),
				FrameRate:  frameRate,
			},
			Audio: mediaserver.MetadataAudio{
				ChannelCount: audioChannels,
				Codec:        audioCodec,
			},
			Transcoding: transcoding,
			BitrateKbps: bitrateKbps,
		},
	}
}

func episodeOr(isEpisode bool, v *float64) *float64 {
	if !isEpisode {
		return nil
	}
	return v
}

// episodeNameRaw mirrors `isEpisode ? EpisodeTitle : null` with the zod
// nullish tri-state (undefined drops the member, null emits null).
func episodeNameRaw(isEpisode bool, raw json.RawMessage) json.RawMessage {
	if !isEpisode {
		return []byte("null")
	}
	return raw
}

func truthyResolution(width, height *float64) *mediaserver.Resolution {
	if width != nil && *width != 0 && height != nil && *height != 0 {
		return &mediaserver.Resolution{Width: *width, Height: *height}
	}
	return nil
}

func playStateTicks(ps *playStateDTO) *float64 {
	if ps == nil {
		return nil
	}
	return ps.PositionTicks
}

func coalesce(values ...*string) string {
	for _, v := range values {
		if v != nil {
			return *v
		}
	}
	return ""
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// --- upstream response shapes (sessionSchema, emby-integration.ts:29-63) ---

type sessionDTO struct {
	ID              *string         `json:"Id"`
	UserID          *string         `json:"UserId"`
	Client          *string         `json:"Client"`
	DeviceID        *string         `json:"DeviceId"`
	DeviceName      *string         `json:"DeviceName"`
	UserName        *string         `json:"UserName"`
	RemoteEndPoint  *string         `json:"RemoteEndPoint"`
	NowPlayingItem  *nowPlayingDTO  `json:"NowPlayingItem"`
	PlayState       *playStateDTO   `json:"PlayState"`
	TranscodingInfo *transcodingDTO `json:"TranscodingInfo"`
}

type nowPlayingDTO struct {
	Type              string          `json:"Type"`
	SeriesName        *string         `json:"SeriesName"`
	Name              *string         `json:"Name"`
	SeasonName        *string         `json:"SeasonName"`
	ParentIndexNumber *float64        `json:"ParentIndexNumber"`
	EpisodeTitle      json.RawMessage `json:"EpisodeTitle"`
	IndexNumber       *float64        `json:"IndexNumber"`
	Album             *string         `json:"Album"`
	EpisodeCount      json.RawMessage `json:"EpisodeCount"`
	RunTimeTicks      *float64        `json:"RunTimeTicks"`
	Width             *float64        `json:"Width"`
	Height            *float64        `json:"Height"`
	MediaStreams      []mediaStream   `json:"MediaStreams"`
	MediaSources      []mediaSource   `json:"MediaSources"`
}

type mediaStream struct {
	BitRate *float64 `json:"BitRate"`
}

type mediaSource struct {
	Bitrate *float64 `json:"Bitrate"`
}

type playStateDTO struct {
	IsPaused      *bool    `json:"IsPaused"`
	PositionTicks *float64 `json:"PositionTicks"`
	PlayMethod    *string  `json:"PlayMethod"`
}

type transcodingDTO struct {
	Bitrate       *float64 `json:"Bitrate"`
	Framerate     *float64 `json:"Framerate"`
	AudioChannels *float64 `json:"AudioChannels"`
	AudioCodec    *string  `json:"AudioCodec"`
	VideoCodec    *string  `json:"VideoCodec"`
	Container     *string  `json:"Container"`
	IsVideoDirect *bool    `json:"IsVideoDirect"`
	IsAudioDirect *bool    `json:"IsAudioDirect"`
	Width         *float64 `json:"Width"`
	Height        *float64 `json:"Height"`
}

// Compile-time interface checks.
var (
	_ mediaserver.Client       = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
