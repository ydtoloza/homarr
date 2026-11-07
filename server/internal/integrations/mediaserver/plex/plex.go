// Package plex is the Go port of the upstream PlexIntegration
// (packages/integrations/src/plex/plex-integration.ts), restricted to the
// media-server surface the widget consumes: getCurrentSessionsAsync
// (GET {url}/status/sessions) plus the framework Check (testingAsync: GET
// {url}/prefs, plex-integration.ts:461-474). The IMediaReleasesIntegration
// surface of the same class (getMediaReleasesAsync + image proxy) belongs to
// the mediaReleases widget and is NOT ported here.
//
// Request semantics pinned from plex-integration.ts:
//   - auth: header "X-Plex-Token: <apiKey secret>" on every call
//     (integrationDefs plex secretKinds [["apiKey"]], httpAuth
//     {type: "apiKeyHeader", name: "X-Plex-Token"}),
//   - sessions (:160-264): GET {url}/status/sessions with NO response.ok
//     check — the XML body is parsed whatever the status (a 401 body fails
//     XML parsing upstream, surfacing as the parse error), then
//     MediaContainer.Video + MediaContainer.Track are flattened and entries
//     without a Player element are dropped,
//   - the response is XML (there is no JSON endpoint): parsed with xml2js
//     parseStringPromise upstream (ParseError "Invalid xml format") and
//     encoding/xml here,
//   - NO per-request timeout upstream (fetchWithTrustedCertificatesAsync
//     without options); the deadline comes from the widget layer
//     (DEFAULT_REQUEST_TIMEOUT_MS = 60s).
//
// Mapping pinned from plex-integration.ts:180-263 and interface.ts (see the
// XML types at the bottom): all scalars are attribute strings funneled through
// parseOptionalNumber (:22-29) / parsePlaybackState (:35-37) /
// parseLocation (:31-33) / parseResolution (:39-46); a "transcode" decision is
// the only non-direct stream (isStreamDirect :153-157 — "copy" counts as
// direct).
package plex

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaserver"
)

// Kind is the registry key, the exact "integration"."kind" string upstream
// (packages/definitions/src/integration.ts "plex").
const Kind = "plex"

// Definition returns the metadata of the plex kind.
func Definition() integrations.Definition { return integrations.Definition{Kind: Kind, Name: "Plex"} }

// init self-registers the kind into the default integrations registry (the
// registered factory carries no secrets yet — see INTEGRATIONS.md §5.7).
func init() {
	integrations.Register(Definition(), func(input integrations.Input) (integrations.Integration, error) {
		return New(input, Secrets{}), nil
	})
}

// Secrets carries the decrypted integrationSecret values plex consumes.
type Secrets struct {
	// APIKey is the secret of kind "apiKey" (the X-Plex-Token).
	APIKey string
}

// SecretsFromMap builds Secrets from a decrypted kind->value map.
func SecretsFromMap(m map[string]string) (Secrets, error) {
	apiKey, err := mediaserver.SecretValue(m, "apiKey")
	return Secrets{APIKey: apiKey}, err
}

// Integration is one Plex instance. All methods are safe for concurrent use.
type Integration struct {
	input   integrations.Input
	secrets Secrets
	urls    mediaorganizer.URLBuilder
	http    mediaorganizer.HTTPClient
}

// New builds one Plex client. An empty apiKey is reported at call time as
// the upstream missing-secret error (getSecretValue,
// base/integration.ts:58-64).
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

// Check implements integrations.Integration: GET {url}/prefs with the
// X-Plex-Token header and Accept: application/json (testingAsync,
// plex-integration.ts:461-474). The CheckResult carries statusCode on
// definitive responses and the taxonomy message on transport failures (the
// upstream test-connection subtypes land with that service).
func (i *Integration) Check(ctx context.Context) (integrations.CheckResult, error) {
	header := i.tokenHeader()
	if header == nil {
		return integrations.CheckResult{Error: (&mediaorganizer.MissingSecretError{Kind: "apiKey"}).Error()}, nil
	}
	url, err := i.urls.Build("/prefs", nil)
	if err != nil {
		return integrations.CheckResult{Error: err.Error()}, nil
	}
	header.Set("Accept", "application/json")
	res, err := i.http.Do(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		return integrations.CheckResult{Error: mediaserver.MsgRequest}, nil
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return integrations.CheckResult{
			StatusCode: res.StatusCode,
			Error:      mediaserver.MsgResponse,
		}, nil
	}
	return integrations.CheckResult{StatusCode: res.StatusCode}, nil
}

// tokenHeader returns the auth header, or nil when the apiKey secret is
// missing (getSecretValue throws upstream).
func (i *Integration) tokenHeader() http.Header {
	if i.secrets.APIKey == "" {
		return nil
	}
	return http.Header{"X-Plex-Token": []string{i.secrets.APIKey}}
}

// GetCurrentSessions ports getCurrentSessionsAsync
// (plex-integration.ts:160-264). showOnlyPlaying is ignored upstream
// (_options).
func (i *Integration) GetCurrentSessions(ctx context.Context, input mediaserver.CurrentSessionsInput) ([]mediaserver.StreamSession, error) {
	header := i.tokenHeader()
	if header == nil {
		return nil, &mediaorganizer.MissingSecretError{Kind: "apiKey"}
	}
	url, err := i.urls.Build("/status/sessions", nil)
	if err != nil {
		return nil, err
	}
	// No response.ok check upstream: read the body whatever the status.
	res, err := i.http.Do(ctx, http.MethodGet, url, header, nil)
	if err != nil {
		return nil, mediaserver.NewRequestError(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, mediaserver.NewParseError(err)
	}

	var container sessionsXML
	if err := xml.Unmarshal(raw, &container); err != nil {
		// Upstream ParseError carries the custom message "Invalid xml format"
		// (plex-integration.ts:476-489); the message never reaches the wire
		// (the widget query replaces every failure with the public constant),
		// so the shared taxonomy message is kept for consistency.
		return nil, mediaserver.NewParseError(errors.New("Invalid xml format"))
	}

	// [Video ?? [], Track ?? []].flat()
	elements := make([]sessionXML, 0, len(container.Video)+len(container.Track))
	elements = append(elements, container.Video...)
	elements = append(elements, container.Track...)
	// no sessions are open or available
	if len(elements) == 0 {
		return []mediaserver.StreamSession{}, nil
	}

	sessions := make([]mediaserver.StreamSession, 0, len(elements))
	for _, element := range elements {
		if session, ok := sessionOf(element); ok {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

// sessionOf maps one XML element (:180-263); entries without a Player element
// are dropped (the upstream mapper returns undefined and filters it).
func sessionOf(element sessionXML) (mediaserver.StreamSession, bool) {
	if len(element.Player) == 0 {
		return mediaserver.StreamSession{}, false
	}
	player := element.Player[0]

	var (
		user      *userXML
		sessionEl *sessionElemXML
		transcode *transcodeXML
		mediaInfo *mediaXML
	)
	if len(element.User) > 0 {
		user = &element.User[0]
	}
	if len(element.SessionElem) > 0 {
		sessionEl = &element.SessionElem[0]
	}
	if len(element.TranscodeSession) > 0 {
		transcode = &element.TranscodeSession[0]
	}
	if len(element.Media) > 0 {
		mediaInfo = &element.Media[0]
	}

	playbackState := parsePlaybackState(player.State)

	var location *string
	if sessionEl != nil {
		location = parseLocation(sessionEl.Location)
	}

	var streams []streamXML
	if mediaInfo != nil && len(mediaInfo.Part) > 0 {
		streams = mediaInfo.Part[0].Stream
	}
	var videoStream, audioStream *streamXML
	for idx := range streams {
		switch streams[idx].StreamType {
		case "1":
			if videoStream == nil {
				videoStream = &streams[idx]
			}
		case "2":
			if audioStream == nil {
				audioStream = &streams[idx]
			}
		}
	}

	isVideoDirect := isStreamDirect(transcodeDecision(transcode, (*transcodeXML).videoDecision))
	isAudioDirect := isStreamDirect(transcodeDecision(transcode, (*transcodeXML).audioDecision))
	containerChanged := transcodeContainerDecisionChanged(transcode, mediaInfo)

	// parseOptionalNumber(session?.$.bandwidth) ?? parseOptionalNumber
	// (media?.$.bitrate) (:210-211) — ?? skips null, so an empty-bandwidth
	// attribute (parses to null) still falls through to the media bitrate.
	bitrateKbps := parseOptionalNumber(deref(sessionBandwidth(sessionEl)))
	if bitrateKbps == nil {
		bitrateKbps = parseOptionalNumber(deref(mediaBitrate(mediaInfo)))
	}

	userID, username := "Anonymous", "Anonymous"
	var profilePictureURL *string
	if user != nil {
		if user.ID != "" {
			userID = user.ID
		}
		if user.Title != "" {
			username = user.Title
		}
		profilePictureURL = user.Thumb
	}

	currentlyPlaying := &mediaserver.CurrentlyPlaying{
		Type:         currentlyPlayingType(element),
		Name:         nameOf(element),
		SeasonName:   element.ParentTitle,
		SeasonNumber: nil,
		EpisodeName:  mediaserver.NullValue(),
		// type === "track" ? (parentTitle ?? null) : null — always present.
		AlbumName:    trackAlbumName(element),
		EpisodeCount: mediaserver.OfValue(parseOptionalNumber(deref(element.Index))),
		Playback: mediaserver.Playback{
			State:      playbackState,
			PositionMs: parseOptionalNumber(element.ViewOffset),
			DurationMs: parseOptionalNumber(element.Duration),
		},
		Location: location,
		Metadata: &mediaserver.Metadata{
			Video: mediaserver.MetadataVideo{
				Resolution: parseResolution(deref(streamWidth(videoStream)), deref(streamHeight(videoStream))),
				FrameRate:  parseOptionalNumber(deref(streamFrameRate(videoStream))),
			},
			Audio: mediaserver.MetadataAudio{
				ChannelCount: parseOptionalNumber(deref(streamChannels(audioStream))),
				Codec:        audioCodecOf(audioStream, mediaInfo),
			},
			Transcoding: mediaserver.Transcoding{
				Container:        transcodeContainerOf(transcode),
				Resolution:       parseResolution(deref(transcodeWidth(transcode)), deref(transcodeHeight(transcode))),
				Target:           transcodeTarget(transcode),
				IsVideoDirect:    isVideoDirect,
				IsAudioDirect:    isAudioDirect,
				ContainerChanged: containerChanged,
			},
			BitrateKbps: bitrateKbps,
		},
	}
	if element.Type == "episode" {
		currentlyPlaying.SeasonNumber = parseOptionalNumber(deref(element.ParentIndex))
		// type === "episode" ? (title ?? null) : null — always present.
		if element.Title != nil {
			currentlyPlaying.EpisodeName = mediaserver.OfValue(*element.Title)
		}
		currentlyPlaying.EpisodeNumber = parseOptionalNumber(deref(element.Index))
	}

	sessionID := "unknown"
	if sessionEl != nil && sessionEl.ID != "" {
		sessionID = sessionEl.ID
	}

	return mediaserver.StreamSession{
		SessionID:   sessionID,
		SessionName: player.Product + " (" + player.Title + ")",
		User: mediaserver.SessionUser{
			UserID:            userID,
			Username:          username,
			ProfilePictureURL: profilePictureURL,
		},
		CurrentlyPlaying: currentlyPlaying,
	}, true
}

// trackAlbumName mirrors `type === "track" ? (parentTitle ?? null) : null`
// (:228): always present, null unless a track with a parent title.
func trackAlbumName(element sessionXML) *string {
	if element.Type != "track" {
		return nil
	}
	return element.ParentTitle
}

// parseOptionalNumber delegates to the shared parser (the plex-integration
// parseOptionalNumber :22-29).
func parseOptionalNumber(value string) *float64 { return mediaserver.ParseOptionalNumber(value) }

// currentlyPlayingType mirrors getCurrentlyPlayingType
// (plex-integration.ts:491-502) plus the live override (:222).
func currentlyPlayingType(element sessionXML) string {
	if element.Live == "1" {
		return mediaserver.TypeTV
	}
	switch element.Type {
	case "movie":
		return mediaserver.TypeMovie
	case "episode":
		return mediaserver.TypeVideo
	case "track":
		return mediaserver.TypeAudio
	default:
		return mediaserver.TypeVideo
	}
}

// nameOf mirrors `grandparentTitle ?? title ?? "Unknown"` (:223).
func nameOf(element sessionXML) string {
	if element.GrandparentTitle != nil {
		return *element.GrandparentTitle
	}
	if element.Title != nil {
		return *element.Title
	}
	return "Unknown"
}

// isStreamDirect mirrors isStreamDirect (:153-157): only "transcode"
// re-encodes; "copy" (remux without re-encoding) and an absent decision count
// as direct.
func isStreamDirect(decision *string) bool {
	return decision == nil || *decision != "transcode"
}

func parsePlaybackState(value string) *string {
	switch value {
	case mediaserver.StatePlaying, mediaserver.StatePaused, mediaserver.StateBuffering:
		return mediaserver.Ptr(value)
	default:
		return nil
	}
}

func parseLocation(value *string) *string {
	if value == nil {
		return nil
	}
	if *value == mediaserver.LocationLAN || *value == mediaserver.LocationWAN {
		return mediaserver.Ptr(*value)
	}
	return nil
}

// parseResolution mirrors parseResolution (:39-46): both numbers must parse.
func parseResolution(width, height string) *mediaserver.Resolution {
	w := mediaserver.ParseOptionalNumber(width)
	h := mediaserver.ParseOptionalNumber(height)
	if w != nil && h != nil {
		return &mediaserver.Resolution{Width: *w, Height: *h}
	}
	return nil
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// transcodeDecision reads one decision attribute off a nilable transcode
// element (`transcodeElement?.$.videoDecision` etc.).
func transcodeDecision(t *transcodeXML, get func(*transcodeXML) *string) *string {
	if t == nil {
		return nil
	}
	return get(t)
}

// The `element?.$.attr` readers below are nil-safe (nil receiver -> nil), so
// the mapper chains translate one to one. A present attribute wins even when
// empty — ?? does not skip ""; deref collapses it to "" which parses to null
// exactly like the upstream `!value` guard.
func (t *transcodeXML) videoDecision() *string { return t.VideoDecision }
func (t *transcodeXML) audioDecision() *string { return t.AudioDecision }
func transcodeContainerOf(t *transcodeXML) *string {
	if t == nil {
		return nil
	}
	return t.Container
}
func transcodeWidth(t *transcodeXML) *string {
	if t == nil {
		return nil
	}
	return t.Width
}
func transcodeHeight(t *transcodeXML) *string {
	if t == nil {
		return nil
	}
	return t.Height
}
func streamWidth(s *streamXML) *string {
	if s == nil {
		return nil
	}
	return s.Width
}
func streamHeight(s *streamXML) *string {
	if s == nil {
		return nil
	}
	return s.Height
}
func streamFrameRate(s *streamXML) *string {
	if s == nil {
		return nil
	}
	return s.FrameRate
}
func streamChannels(s *streamXML) *string {
	if s == nil {
		return nil
	}
	return s.Channels
}
func sessionBandwidth(s *sessionElemXML) *string {
	if s == nil {
		return nil
	}
	return s.Bandwidth
}
func mediaBitrate(m *mediaXML) *string {
	if m == nil {
		return nil
	}
	return m.Bitrate
}

func transcodeContainerDecisionChanged(t *transcodeXML, m *mediaXML) bool {
	if t == nil || m == nil {
		return false
	}
	return t.Container != nil && m.Container != nil && *t.Container != *m.Container
}

// audioCodecOf mirrors `audioStream?.$.codec ?? mediaInfoElement?.$.audioCodec
// ?? null` (:243): presence (not emptiness) decides, ?? does not skip "".
func audioCodecOf(s *streamXML, m *mediaXML) *string {
	if s != nil && s.Codec != nil {
		return s.Codec
	}
	if m != nil && m.AudioCodec != nil {
		return m.AudioCodec
	}
	return nil
}

func transcodeTarget(t *transcodeXML) mediaserver.Target {
	if t == nil {
		return mediaserver.Target{}
	}
	return mediaserver.Target{AudioCodec: t.AudioCodec, VideoCodec: t.VideoCodec}
}

// --- upstream response shapes (interface.ts, xml2js explicitArray layout) ---

// sessionsXML mirrors PlexResponse (interface.ts:93-95): the Video and Track
// child arrays of MediaContainer. Attribute pointers are nil when absent —
// the xml2js `undefined` that the ?? chains rely on.
type sessionsXML struct {
	Video []sessionXML `xml:"Video"`
	Track []sessionXML `xml:"Track"`
}

// sessionXML mirrors Session (interface.ts:54-91).
type sessionXML struct {
	GrandparentTitle *string `xml:"grandparentTitle,attr"`
	ParentTitle      *string `xml:"parentTitle,attr"`
	ParentIndex      *string `xml:"parentIndex,attr"`
	Title            *string `xml:"title,attr"`
	Index            *string `xml:"index,attr"`
	Type             string  `xml:"type,attr"`
	Live             string  `xml:"live,attr"`
	ViewOffset       string  `xml:"viewOffset,attr"`
	Duration         string  `xml:"duration,attr"`

	User             []userXML        `xml:"User"`
	Player           []playerXML      `xml:"Player"`
	SessionElem      []sessionElemXML `xml:"Session"`
	TranscodeSession []transcodeXML   `xml:"TranscodeSession"`
	Media            []mediaXML       `xml:"Media"`
}

// userXML mirrors User.$. (interface.ts:55-61).
type userXML struct {
	ID    string  `xml:"id,attr"`
	Title string  `xml:"title,attr"`
	Thumb *string `xml:"thumb,attr"`
}

// playerXML mirrors Player.$. (interface.ts:62-70).
type playerXML struct {
	Product string `xml:"product,attr"`
	Title   string `xml:"title,attr"`
	State   string `xml:"state,attr"`
}

// sessionElemXML mirrors Session.$. (interface.ts:71-77).
type sessionElemXML struct {
	ID        string  `xml:"id,attr"`
	Bandwidth *string `xml:"bandwidth,attr"`
	Location  *string `xml:"location,attr"`
}

// transcodeXML mirrors TranscodeSession.$. (interface.ts:37-52).
type transcodeXML struct {
	VideoDecision *string `xml:"videoDecision,attr"`
	AudioDecision *string `xml:"audioDecision,attr"`
	Container     *string `xml:"container,attr"`
	AudioCodec    *string `xml:"audioCodec,attr"`
	VideoCodec    *string `xml:"videoCodec,attr"`
	Width         *string `xml:"width,attr"`
	Height        *string `xml:"height,attr"`
}

// mediaXML mirrors Media (interface.ts:26-35).
type mediaXML struct {
	AudioCodec *string   `xml:"audioCodec,attr"`
	Bitrate    *string   `xml:"bitrate,attr"`
	Container  *string   `xml:"container,attr"`
	Part       []partXML `xml:"Part"`
}

// partXML mirrors Part (interface.ts:19-24).
type partXML struct {
	Container *string     `xml:"container,attr"`
	Stream    []streamXML `xml:"Stream"`
}

// streamXML mirrors Stream.$. (interface.ts:6-17).
type streamXML struct {
	StreamType string  `xml:"streamType,attr"`
	Codec      *string `xml:"codec,attr"`
	Width      *string `xml:"width,attr"`
	Height     *string `xml:"height,attr"`
	FrameRate  *string `xml:"frameRate,attr"`
	Channels   *string `xml:"channels,attr"`
}

// Compile-time interface checks.
var (
	_ mediaserver.Client       = (*Integration)(nil)
	_ integrations.Integration = (*Integration)(nil)
)
