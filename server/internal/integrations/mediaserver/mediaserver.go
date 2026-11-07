// Package mediaserver is the Go port of the shared media-server abstraction
// of upstream (packages/integrations/src/interfaces/media-server + the per-app
// clients under packages/integrations/src/{plex,jellyfin,emby}): the "now
// playing" stream sessions the media-server widget consumes.
//
// Upstream interface (media-server-integration.ts:3-6):
//
//	interface IMediaServerIntegration {
//	  getCurrentSessionsAsync(options: CurrentSessionsInput): Promise<StreamSession[]>;
//	}
//
// The Go mirror is the Client interface below; the per-app versioned clients
// live in subpackages that self-register their kind into the integrations
// registry (internal/integrations/integrations.go), mirroring the upstream
// layout <app>/<app>-integration.ts. Only the servers the media-server widget
// needs are ported here: plex, jellyfin and emby. Navidrome also implements
// IMediaServerIntegration upstream (category mediaService) but is out of this
// batch; the widget factory fails that kind per integration until it lands,
// exactly like sportarr for the media-organizer widget. Upstream's
// IMediaReleasesIntegration (the separate mediaReleases widget surface,
// packages/api/src/router/widgets/media-release.ts) is NOT part of this
// package.
//
// The widget data surface is the tRPC query widget.mediaServer.getCurrentStreams
// (packages/api/src/router/widgets/media-server.ts:10-46): NO websocket or bus
// event exists for these sessions upstream (apps/websocket has no media
// channel; the frontend polls every 10s, packages/widgets/src/
// refetch-intervals.ts:18), so this port adds no bus topics either.
//
// JSON shape discipline: the wire type is StreamSession
// (media-server-types.ts:1-53) serialized by superjson, which drops undefined
// members and keeps null ones. Fields the upstream mappers always produce
// (null or value) are pointers WITHOUT omitempty; fields that upstream leaves
// undefined are pointers WITH omitempty; the genuinely tri-state members
// (absent | null | value — jellyfin/emby episodeName and episodeCount) carry
// the Optional interface below.
package mediaserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// CurrentlyPlaying type values (media-server-types.ts:10).
const (
	TypeAudio = "audio"
	TypeVideo = "video"
	TypeTV    = "tv"
	TypeMovie = "movie"
)

// Playback state values (media-server-types.ts:19) plus the null case.
const (
	StatePlaying   = "playing"
	StatePaused    = "paused"
	StateBuffering = "buffering"
)

// Location values (media-server-types.ts:23).
const (
	LocationLAN = "lan"
	LocationWAN = "wan"
)

// StreamSession mirrors upstream StreamSession (media-server-types.ts:1-53).
// currentlyPlaying is null when the server reports no playing item
// (jellyfin/emby) and always an object for plex.
type StreamSession struct {
	SessionID        string            `json:"sessionId"`
	SessionName      string            `json:"sessionName"`
	User             SessionUser       `json:"user"`
	CurrentlyPlaying *CurrentlyPlaying `json:"currentlyPlaying"`
}

// SessionUser mirrors the user member of StreamSession; profilePictureUrl is
// string|null upstream (always present).
type SessionUser struct {
	UserID            string  `json:"userId"`
	Username          string  `json:"username"`
	ProfilePictureURL *string `json:"profilePictureUrl"`
}

// CurrentlyPlaying mirrors the currentlyPlaying member
// (media-server-types.ts:9-52). seasonName is string|undefined upstream —
// dropped from the JSON when absent (plex passes parentTitle through).
type CurrentlyPlaying struct {
	Type          string    `json:"type"`
	Name          string    `json:"name"`
	SeasonName    *string   `json:"seasonName,omitempty"`
	SeasonNumber  *float64  `json:"seasonNumber"`
	EpisodeName   Optional  `json:"episodeName,omitempty"`
	EpisodeNumber *float64  `json:"episodeNumber"`
	AlbumName     *string   `json:"albumName"`
	EpisodeCount  Optional  `json:"episodeCount,omitempty"`
	Playback      Playback  `json:"playback"`
	Location      *string   `json:"location"`
	Metadata      *Metadata `json:"metadata"`
}

// Playback mirrors playback (media-server-types.ts:18-22): all members are
// value-or-null, never absent.
type Playback struct {
	State      *string  `json:"state"`
	PositionMs *float64 `json:"positionMs"`
	DurationMs *float64 `json:"durationMs"`
}

// Metadata mirrors the metadata member (media-server-types.ts:24-51): null
// upstream when the client provides none, so the pointer stays without
// omitempty and the subtree below is always emitted.
type Metadata struct {
	Video       MetadataVideo `json:"video"`
	Audio       MetadataAudio `json:"audio"`
	Transcoding Transcoding   `json:"transcoding"`
	BitrateKbps *float64      `json:"bitrateKbps"`
}

// MetadataVideo mirrors metadata.video; resolution is {width,height}|null and
// frameRate number|null.
type MetadataVideo struct {
	Resolution *Resolution `json:"resolution"`
	FrameRate  *float64    `json:"frameRate"`
}

// MetadataAudio mirrors metadata.audio.
type MetadataAudio struct {
	ChannelCount *float64 `json:"channelCount"`
	Codec        *string  `json:"codec"`
}

// Transcoding mirrors metadata.transcoding; the booleans are always present.
type Transcoding struct {
	Container        *string     `json:"container"`
	Resolution       *Resolution `json:"resolution"`
	Target           Target      `json:"target"`
	IsVideoDirect    bool        `json:"isVideoDirect"`
	IsAudioDirect    bool        `json:"isAudioDirect"`
	ContainerChanged bool        `json:"containerChanged"`
}

// Target mirrors metadata.transcoding.target.
type Target struct {
	AudioCodec *string `json:"audioCodec"`
	VideoCodec *string `json:"videoCodec"`
}

// Resolution mirrors {width: number; height: number}.
type Resolution struct {
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// CurrentSessionsInput mirrors CurrentSessionsInput
// (media-server-types.ts:55-57): the widget sends showOnlyPlaying.
type CurrentSessionsInput struct {
	ShowOnlyPlaying bool
}

// Client is the Go mirror of IMediaServerIntegration. Implementations must be
// safe for concurrent use (the widget procedure fans out over integrations).
// Errors are *Error values from this package so callers can distinguish
// transport failures from bad responses.
type Client interface {
	// GetCurrentSessions is getCurrentSessionsAsync: the active streams.
	GetCurrentSessions(ctx context.Context, input CurrentSessionsInput) ([]StreamSession, error)
}

// Optional is a tri-state JSON member: the zero value is ABSENT (the key is
// omitted from the wire, like superjson dropping undefined — encoding/json
// omits a nil interface under omitempty), NullValue emits an explicit null,
// and OfValue emits the value. It exists for the members upstream maps as
// `x ?? null` in one server but passes through raw in another
// (jellyfin/emby episodeName and episodeCount).
type Optional interface {
	json.Marshaler
}

type nullOptional struct{}

func (nullOptional) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

type valueOptional struct{ raw json.RawMessage }

func (v valueOptional) MarshalJSON() ([]byte, error) { return v.raw, nil }

// NullValue returns the explicit-null state of Optional.
func NullValue() Optional { return nullOptional{} }

// OfValue returns the value state of Optional (marshaled eagerly so the
// payload is stable).
func OfValue(v any) Optional {
	raw, err := json.Marshal(v)
	if err != nil {
		// StreamSession fields are plain JSON scalars; a marshal failure is a
		// programming error surfaced at emit time.
		panic(fmt.Sprintf("mediaserver: OfValue: %v", err))
	}
	return valueOptional{raw: raw}
}

// OptionalNullish decodes one tri-state JSON member from raw wire JSON:
// absent (no key) -> nil, null -> NullValue, value -> OfValue.
func OptionalNullish(raw json.RawMessage) Optional {
	if len(raw) == 0 {
		return nil
	}
	if string(raw) == "null" {
		return NullValue()
	}
	return valueOptional{raw: append(json.RawMessage{}, raw...)}
}

// Re-export the shared error taxonomy of the integrations domain so callers
// of every domain see one vocabulary (the upstream classes are shared too).
type (
	Error              = mediaorganizer.Error
	ErrorKind          = mediaorganizer.ErrorKind
	MissingSecretError = mediaorganizer.MissingSecretError
)

const (
	ErrKindRequest  = mediaorganizer.ErrKindRequest
	ErrKindResponse = mediaorganizer.ErrKindResponse
	ErrKindParse    = mediaorganizer.ErrKindParse
	ErrKindUnknown  = mediaorganizer.ErrKindUnknown

	MsgRequest  = mediaorganizer.MsgRequest
	MsgResponse = mediaorganizer.MsgResponse
	MsgParse    = mediaorganizer.MsgParse
	MsgUnknown  = mediaorganizer.MsgUnknown
)

// Re-exported constructors and helpers of the shared taxonomy.
var (
	NewRequestError  = mediaorganizer.NewRequestError
	NewResponseError = mediaorganizer.NewResponseError
	NewParseError    = mediaorganizer.NewParseError
	NewUnknownError  = mediaorganizer.NewUnknownError
	AsError          = mediaorganizer.AsError
	SecretValue      = mediaorganizer.SecretValue
)

// IntegrationRef is one resolved integration row as the widget router hands
// it to the client factories (mirrors mediaorganizer.IntegrationRef). Secrets
// carries the DECRYPTED secret values by kind ("apiKey", "username",
// "password").
type IntegrationRef struct {
	ID          string
	Name        string
	Kind        string
	URL         string
	ExternalURL string
	Secrets     map[string]string
}

// Input converts the ref into the framework constructor input.
func (r IntegrationRef) Input() integrations.Input {
	return integrations.Input{ID: r.ID, Kind: r.Kind, Name: r.Name, URL: r.URL, ExternalURL: r.ExternalURL}
}

// ParseOptionalNumber ports parseOptionalNumber
// (plex-integration.ts:22-29): "" or absent -> null, garbage -> null.
func ParseOptionalNumber(value string) *float64 {
	if value == "" {
		return nil
	}
	var f float64
	if err := json.Unmarshal([]byte(value), &f); err != nil {
		return nil
	}
	return &f
}

// Ptr is a small helper for optional fields (mirrors mediaorganizer.Ptr, kept
// local so mediaserver does not drag the media-organizer domain types in).
func Ptr[T any](v T) *T { return &v }
