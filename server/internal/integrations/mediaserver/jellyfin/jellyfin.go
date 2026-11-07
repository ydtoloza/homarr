// Package jellyfin is the Go port of the upstream JellyfinIntegration
// (packages/integrations/src/jellyfin/jellyfin-integration.ts), restricted to
// the media-server surface the widget consumes: getCurrentSessionsAsync
// (GET {url}/Sessions through the @jellyfin/sdk getSessionApi) plus the
// framework Check (testingAsync: GET {url}/System/Ping, possibly preceded by
// POST {url}/Users/AuthenticateByName). The IMediaReleasesIntegration surface
// of the same class is NOT ported here.
//
// Request semantics pinned from jellyfin-integration.ts and the @jellyfin/sdk
// behavior it drives:
//   - auth (getAccessTokenAsync :100-130): the "apiKey" secret is used
//     directly as the access token; otherwise username/password authenticate
//     via POST /Users/AuthenticateByName with the fixed Authorization prefix
//     `MediaBrowser Client="Homarr", Device="Homarr", DeviceId="homarr",
//     Version="0.0.1"` and body {"Username","Pw"}, redirect: "error"; every
//     failure collapses to the plain Error "Jellyfin authentication failed".
//     Concurrent authentications of the same identity (url+username+password
//     fingerprint) share one in-flight request (pendingAuthentications :88,
//     module-level upstream; per-instance here — same identity scope).
//   - every authenticated call carries
//     `Authorization: <prefix>, Token="<accessToken>"` (the SDK Api client
//     header, getHttpAuthenticationAsync :92-98),
//   - sessions (:150-242): GET /Sessions (no query params), axios semantics —
//     non-2xx rejects (IntegrationResponseError via
//     integrationAxiosHttpErrorHandler), transport failures are
//     IntegrationRequestError,
//   - filters (:155-158): UserId must be present, DeviceId !== "homarr"
//     (nullish DeviceId is KEPT — null !== "homarr"), and showOnlyPlaying
//     requires NowPlayingItem,
//   - location: jellyfin reports no lan/wan — the client classifies the
//     RemoteEndPoint IP itself (parseLocation :54-66, private ranges
//     :34-42). Node isIP classifies an IPv4-mapped IPv6 literal as 6; the
//     "::ffff:" unwrap then reclassifies to 4 before the range check (:58-63),
//   - NO per-request timeout upstream (axios certificate instance, no
//     timeout option); the widget layer deadline (60s) applies.
package jellyfin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"strings"
	"sync"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "jellyfin").
const Kind = "jellyfin"

// deviceID is the DeviceId of the homarr sessions — the SDK deviceInfo
// (:132-141) and the session filter (:157).
const deviceID = "homarr"

// authorizationPrefix is the fixed prefix of every Authorization header (:87).
const authorizationPrefix = `MediaBrowser Client="Homarr", Device="Homarr", DeviceId="homarr", Version="0.0.1"`

// authFailedMessage is the pinned message of every username/password
// authentication failure of the query path (:114-120).
const authFailedMessage = "Jellyfin authentication failed"

// Definition returns the metadata of the jellyfin kind.
func Definition() integrations.Definition {
	return integrations.Definition{Kind: Kind, Name: "Jellyfin"}
}

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values jellyfin consumes
// (integrationDefs jellyfin secretKinds [["username","password"],["apiKey"]]):
// EITHER the apiKey alone OR the username/password pair.
type Secrets struct {
	APIKey   string
	Username string
	Password string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map (presence
// semantics, like hasSecretValue).
func SecretsFromMap(m map[string]string) (Secrets, error) {
	out := Secrets{APIKey: m["apiKey"], Username: m["username"], Password: m["password"]}
	if _, ok := m["apiKey"]; ok {
		return out, nil
	}
	if _, ok := m["username"]; !ok {
		return Secrets{}, &mediaorganizer.MissingSecretError{Kind: "username"}
	}
	if _, ok := m["password"]; !ok {
		return Secrets{}, &mediaorganizer.MissingSecretError{Kind: "password"}
	}
	return out, nil
}

// Integration is one Jellyfin instance. All methods are safe for concurrent
// use; authentications dedupe through the in-flight map.
type Integration struct {
	input   integrations.Input
	secrets Secrets
	urls    mediaorganizer.URLBuilder
	http    mediaorganizer.HTTPClient

	mu       sync.Mutex
	inflight map[string]*authCall
}

type authCall struct {
	done  chan struct{}
	token string
	err   error
}

// New builds one Jellyfin client.
func New(input integrations.Input, secrets Secrets) *Integration {
	return &Integration{
		input:    input,
		secrets:  secrets,
		urls:     mediaorganizer.URLBuilder{URL: input.URL, ExternalURL: input.ExternalURL},
		inflight: make(map[string]*authCall),
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

// Check implements integrations.Integration (testingAsync :143-148): with an
// apiKey only GET /System/Ping runs; with username/password the SDK client
// authenticates first (authenticateUserByName :354-357), so POST
// /Users/AuthenticateByName precedes the ping. Both requests classify through
// the axios error taxonomy (integrationAxiosHttpErrorHandler), which is why
// the Check login reports the shared request/response messages instead of the
// query-path "Jellyfin authentication failed" wrapper.
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	token, err := i.accessTokenForCheck(ctx)
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.AsError(err).Error()}, nil
	}
	res, err := i.getWithToken(ctx, "/System/Ping", token)
	if err != nil {
		return integrations.CheckResult{Error: mediaorganizer.AsError(err).Error()}, nil
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

// accessTokenForCheck resolves the token for the Check flow: the apiKey (no
// request) or a classified AuthenticateByName round trip.
func (i *Integration) accessTokenForCheck(ctx context.Context) (string, error) {
	if i.hasAPIKey() {
		return i.secrets.APIKey, nil
	}
	return i.authenticateByNameClassified(ctx)
}

// GetCurrentSessions ports getCurrentSessionsAsync (:150-242).
func (i *Integration) GetCurrentSessions(ctx context.Context, input mediaserver.CurrentSessionsInput) ([]mediaserver.StreamSession, error) {
	token, err := i.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	res, err := i.getWithToken(ctx, "/Sessions", token)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, mediaserver.NewParseError(err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, mediaserver.NewResponseError(fmt.Errorf("status %d for %s", res.StatusCode, "/Sessions"))
	}
	var sessions []sessionDTO
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, mediaserver.NewParseError(err)
	}

	out := make([]mediaserver.StreamSession, 0, len(sessions))
	for idx := range sessions {
		info := &sessions[idx]
		// .filter(sessionInfo => sessionInfo.UserId !== undefined)
		if info.UserID == nil {
			continue
		}
		// .filter(sessionInfo => sessionInfo.DeviceId !== "homarr") — a nullish
		// DeviceId is kept (null !== "homarr").
		if info.DeviceID != nil && *info.DeviceID == deviceID {
			continue
		}
		// .filter(... || sessionInfo.NowPlayingItem !== undefined)
		if input.ShowOnlyPlaying && info.NowPlayingItem == nil {
			continue
		}
		out = append(out, i.sessionOf(info))
	}
	return out, nil
}

// sessionOf maps one /Sessions entry (:159-241).
func (i *Integration) sessionOf(info *sessionDTO) mediaserver.StreamSession {
	var currentlyPlaying *mediaserver.CurrentlyPlaying
	if info.NowPlayingItem != nil {
		currentlyPlaying = currentlyPlayingOf(info.NowPlayingItem, info.PlayState, info.TranscodingInfo, deref(info.RemoteEndPoint))
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

// currentlyPlayingOf builds the currentlyPlaying member (:162-229). The
// remote endpoint travels separately (it belongs to the session, not the
// playing item).
func currentlyPlayingOf(np *nowPlayingDTO, ps *playStateDTO, tr *transcodingDTO, remoteEndPoint string) *mediaserver.CurrentlyPlaying {
	isEpisode := np.Type == "Episode"

	// playback.state: IsPaused ? "paused" : "playing" — a nullish PlayState
	// plays.
	var state string
	if ps != nil && ps.IsPaused != nil && *ps.IsPaused {
		state = mediaserver.StatePaused
	} else {
		state = mediaserver.StatePlaying
	}

	containerChanged := ps != nil && ps.PlayMethod != nil && *ps.PlayMethod == "DirectStream"

	// No TranscodingInfo: every member falls back to null, the direct flags
	// default to true — and containerChanged still comes from the PlayState
	// (PlayMethod === "DirectStream", :224).
	transcoding := mediaserver.Transcoding{
		Target:           mediaserver.Target{},
		IsVideoDirect:    true,
		IsAudioDirect:    true,
		ContainerChanged: containerChanged,
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

	// bitrate: TranscodingInfo.Bitrate ?? MediaSources[0].Bitrate ??
	// (mediaStreamsBitrateBps || null) — ?? skips only null/undefined, so a 0
	// Bitrate stays 0 (:166-176).
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
		Type:          ConvertType(np.Type),
		Name:          coalesce(np.SeriesName, np.Name),
		SeasonName:    mediaserver.Ptr(coalesce(np.SeasonName)),
		SeasonNumber:  episodeOr(isEpisode, np.ParentIndexNumber),
		EpisodeName:   mediaserver.OptionalNullish(episodeNameRaw(isEpisode, np.EpisodeTitle)),
		EpisodeNumber: episodeOr(isEpisode, np.IndexNumber),
		AlbumName:     mediaserver.Ptr(coalesce(np.Album)),
		EpisodeCount:  mediaserver.OptionalNullish(np.EpisodeCount),
		Playback: mediaserver.Playback{
			State:      mediaserver.Ptr(state),
			PositionMs: TicksToMs(playStateTicks(ps)),
			DurationMs: TicksToMs(np.RunTimeTicks),
		},
		Location: ParseLocation(remoteEndPoint),
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

// convertType ports convertJellyfinType (:366-385): unknown kinds fall to tv.
// ConvertType is the shared BaseItemKind mapping exported for the emby port
// (upstream emby-integration.ts imports convertJellyfinType).
func ConvertType(kind string) string {
	switch kind {
	case "Audio", "MusicVideo":
		return mediaserver.TypeAudio
	case "Episode", "Video":
		return mediaserver.TypeVideo
	case "Movie":
		return mediaserver.TypeMovie
	default:
		return mediaserver.TypeTV
	}
}

// episodeOr mirrors `isEpisode ? (x ?? null) : null`.
func episodeOr(isEpisode bool, v *float64) *float64 {
	if !isEpisode {
		return nil
	}
	return v
}

// episodeNameRaw mirrors `isEpisode ? EpisodeTitle : null`: EpisodeTitle is
// tri-state upstream (undefined drops the member, null emits null).
func episodeNameRaw(isEpisode bool, raw json.RawMessage) json.RawMessage {
	if !isEpisode {
		return []byte("null")
	}
	return raw
}

// truthyResolution mirrors `Width && Height ? {width, height} : null`.
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

// ticksToMs ports ticksToMs (:28-30): null -> null, Math.round(ticks/10000).
// TicksToMs is the shared tick conversion exported for the emby port (upstream
// emby-integration.ts imports it from the jellyfin module).
func TicksToMs(ticks *float64) *float64 {
	if ticks == nil {
		return nil
	}
	return mediaserver.Ptr(math.Round(*ticks / 10000))
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

// --- authentication ---

func (i *Integration) hasAPIKey() bool {
	return i.secrets.APIKey != ""
}

// accessToken ports getAccessTokenAsync (:100-130): apiKey passthrough or the
// deduplicated username/password login.
func (i *Integration) accessToken(ctx context.Context) (string, error) {
	if i.hasAPIKey() {
		return i.secrets.APIKey, nil
	}
	identity := i.authIdentity()
	i.mu.Lock()
	if call, ok := i.inflight[identity]; ok {
		i.mu.Unlock()
		<-call.done
		return call.token, call.err
	}
	call := &authCall{done: make(chan struct{})}
	i.inflight[identity] = call
	i.mu.Unlock()

	call.token, call.err = i.authenticateByName(ctx)
	close(call.done)

	i.mu.Lock()
	delete(i.inflight, identity)
	i.mu.Unlock()
	return call.token, call.err
}

// authIdentity mirrors createKeyedFingerprint(JSON.stringify([url, username,
// password])): the fingerprint only keys the in-flight dedupe, so a plain
// hash of the same fields is equivalent (never serialized).
func (i *Integration) authIdentity() string {
	sum := sha256.Sum256([]byte(i.urls.URL + "\n" + i.secrets.Username + "\n" + i.secrets.Password))
	return hex.EncodeToString(sum[:])
}

// authenticateByName ports the authenticate closure (:108-122); every failure
// collapses to the plain "Jellyfin authentication failed" error (the unknown
// taxonomy).
func (i *Integration) authenticateByName(ctx context.Context) (string, error) {
	token, err := i.authenticateByNameClassified(ctx)
	if err != nil {
		return "", mediaserver.NewUnknownError(errors.New(authFailedMessage))
	}
	return token, nil
}

// authenticateByNameClassified runs the same login with the axios taxonomy —
// the testingAsync path (getApiAsync :348-363 calls
// api.authenticateUserByName, whose axios failures classify through
// integrationAxiosHttpErrorHandler).
func (i *Integration) authenticateByNameClassified(ctx context.Context) (string, error) {
	url, err := i.urls.Build("/Users/AuthenticateByName", nil)
	if err != nil {
		return "", mediaserver.NewRequestError(err)
	}
	body, _ := json.Marshal(authenticateByNameBody{Username: i.secrets.Username, Pw: i.secrets.Password})
	header := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{authorizationPrefix},
	}
	// redirect: "error" — do not follow; a redirect is a failed login.
	client := *i.http.Std()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", mediaserver.NewRequestError(err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", mediaserver.NewRequestError(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return "", mediaserver.NewUnknownError(errors.New(authFailedMessage))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// A redirect (3xx without ErrUseLastResponse error) is the upstream
		// fetch redirect:"error" failure — a request error, not a response
		// error.
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			return "", mediaserver.NewRequestError(fmt.Errorf("unexpected redirect to %s", res.Header.Get("Location")))
		}
		return "", mediaserver.NewResponseError(fmt.Errorf("status %d for %s", res.StatusCode, "/Users/AuthenticateByName"))
	}
	var result struct {
		AccessToken *string `json:"AccessToken"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", mediaserver.NewUnknownError(errors.New(authFailedMessage))
	}
	if result.AccessToken == nil || len(*result.AccessToken) == 0 {
		return "", mediaserver.NewUnknownError(errors.New(authFailedMessage))
	}
	return *result.AccessToken, nil
}

// authenticateByNameBody mirrors JSON.stringify({Username, Pw}) field order.
type authenticateByNameBody struct {
	Username string `json:"Username"`
	Pw       string `json:"Pw"`
}

// getWithToken issues one GET with the SDK Authorization header.
func (i *Integration) getWithToken(ctx context.Context, path, token string) (*http.Response, error) {
	url, err := i.urls.Build(path, nil)
	if err != nil {
		return nil, mediaserver.NewRequestError(err)
	}
	header := http.Header{"Authorization": []string{authorizationPrefix + `, Token="` + token + `"`}}
	res, err := i.http.Do(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		// axios request failure -> IntegrationRequestError.
		return nil, mediaserver.NewRequestError(err)
	}
	return res, nil
}

// --- location classification (jellyfin-integration.ts:33-66) ---

// privatePrefixes mirrors the privateAddresses BlockList (:34-42).
var privatePrefixes = []struct {
	prefix netip.Prefix
	family int
}{
	{mustPrefix("10.0.0.0/8"), 4},
	{mustPrefix("172.16.0.0/12"), 4},
	{mustPrefix("192.168.0.0/16"), 4},
	{mustPrefix("127.0.0.0/8"), 4},
	{mustPrefix("169.254.0.0/16"), 4},
	{mustPrefix("fc00::/7"), 6},
	{mustPrefix("fe80::/10"), 6},
	{mustPrefix("::1/128"), 6},
}

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// nodeIsIP mirrors node:net isIP (0 invalid, 4 IPv4, 6 IPv6 — an IPv4-mapped
// IPv6 literal is 6).
func nodeIsIP(s string) int {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return 0
	}
	if addr.Is4() {
		return 4
	}
	return 6
}

// nodeCheckPrivate mirrors BlockList.check(host, type).
func nodeCheckPrivate(host string, family int) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	for _, p := range privatePrefixes {
		if p.family == family && p.prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// extractHost ports extractHost (:44-52).
func extractHost(remoteEndPoint string) string {
	if strings.HasPrefix(remoteEndPoint, "[") {
		end := strings.Index(remoteEndPoint, "]")
		if end == -1 {
			return remoteEndPoint
		}
		return remoteEndPoint[1:end]
	}
	if strings.Count(remoteEndPoint, ":") == 1 {
		return remoteEndPoint[:strings.LastIndex(remoteEndPoint, ":")]
	}
	return remoteEndPoint
}

// parseLocation ports parseLocation (:54-66): null when the endpoint is
// absent or not an IP, lan for the private ranges, wan otherwise.
// ParseLocation is the shared RemoteEndPoint classifier exported for the emby
// port (upstream emby-integration.ts imports it from the jellyfin module).
func ParseLocation(remoteEndPoint string) *string {
	if remoteEndPoint == "" {
		return nil
	}
	host := extractHost(remoteEndPoint)
	family := nodeIsIP(host)
	if family == 6 && strings.HasPrefix(strings.ToLower(host), "::ffff:") && nodeIsIP(host[7:]) == 4 {
		host = host[7:]
		family = 4
	}
	if family == 0 {
		return nil
	}
	if nodeCheckPrivate(host, family) {
		return mediaserver.Ptr(mediaserver.LocationLAN)
	}
	return mediaserver.Ptr(mediaserver.LocationWAN)
}

// --- upstream response shapes (@jellyfin/sdk generated-client models) ---

// sessionDTO mirrors the SessionInfoDto members the mapper consumes; the
// nullish members are pointers (absent and null both nil where the mapper
// only coalesces) and the tri-state members are RawMessages.
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
