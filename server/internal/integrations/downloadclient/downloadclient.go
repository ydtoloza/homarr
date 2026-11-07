// Package downloadclient is the Go port of the shared download-client
// abstraction of upstream (packages/integrations/src/interfaces/downloads +
// the per-app clients under packages/integrations/src/download-client): the
// normalized jobs/status view the downloads widget consumes from torrent and
// usenet clients.
//
// Upstream interface (download-client-integration.ts:13-28):
//
//	interface IDownloadClientIntegration {
//	  getClientJobsAndStatusAsync(input: { limit: number; selection?: DownloadClientSelection }):
//	    Promise<DownloadClientJobsAndStatus>;
//	  pauseQueueAsync(): Promise<void>;
//	  pauseItemAsync(item: DownloadClientItem): Promise<void>;
//	  resumeQueueAsync(): Promise<void>;
//	  resumeItemAsync(item: DownloadClientItem): Promise<void>;
//	  deleteItemAsync(item: DownloadClientItem, fromDisk: boolean): Promise<void>;
//	}
//
// The Go mirror is the Client interface below; the per-app clients live in
// subpackages that self-register their kind into the integrations registry,
// mirroring the upstream layout download-client/<app>/<app>-integration.ts:
// qbittorrent (kind "qBittorrent"), transmission, deluge and sabnzbd (kind
// "sabNzbd").
package downloadclient

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/integrations"
	"github.com/ydtoloza/homarr/server/internal/integrations/mediaorganizer"
)

// Item type/state enumerations (download-client-items.ts:24, 41).
const (
	TypeTorrent = "torrent"
	TypeUsenet  = "usenet"
	TypeMisc    = "miscellaneous"
)

// State values of DownloadClientItem.state (download-client-items.ts:5-7, 41).
const (
	StateUnknown     = "unknown"
	StateDownloading = "downloading"
	StateQueued      = "queued"
	StatePaused      = "paused"
	StateCompleted   = "completed"
	StateFailed      = "failed"
	StateProcessing  = "processing"
	StateLeeching    = "leeching"
	StateStalled     = "stalled"
	StateSeeding     = "seeding"
)

// Item is the normalized download item (upstream downloadClientItemSchema,
// download-client-items.ts:16-46). Optional members (sent, received,
// downSpeed, upSpeed, added, category) are dropped from the JSON when the
// provider does not supply them; category is a string OR an array of strings
// upstream, so it travels as any (nil interface = absent).
type Item struct {
	ID        string   `json:"id"`
	Index     int64    `json:"index"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Size      float64  `json:"size"`
	Sent      *float64 `json:"sent,omitempty"`
	Received  *float64 `json:"received,omitempty"`
	DownSpeed *float64 `json:"downSpeed,omitempty"`
	UpSpeed   *float64 `json:"upSpeed,omitempty"`
	// Time is positive = eta (0 = infinite), negative = ms since completion
	// (download-client-items.ts:36-37).
	Time float64 `json:"time"`
	// Added is the unix timestamp in MILLISECONDS when the item was added.
	Added *float64 `json:"added,omitempty"`
	State string   `json:"state"`
	// Progress is 0..1 (1 = complete).
	Progress float64 `json:"progress"`
	Category any     `json:"category,omitempty"`
}

// Rates are the client speeds in bytes/s (download-client-status.ts:6-9); up
// is absent for SABnzbd upstream.
type Rates struct {
	Down float64  `json:"down"`
	Up   *float64 `json:"up,omitempty"`
}

// Status is the client status (download-client-status.ts:3-12).
type Status struct {
	Paused bool     `json:"paused"`
	Rates  Rates    `json:"rates"`
	Types  []string `json:"types"`
}

// RatioItem is the original unfiltered accounting population entry
// (download-client-data.ts:8): a Pick of the item fields the widget derives
// the ratio from.
type RatioItem struct {
	Sent     *float64 `json:"sent,omitempty"`
	Size     float64  `json:"size"`
	Progress float64  `json:"progress"`
	Category any      `json:"category,omitempty"`
}

// JobsAndStatus is the widget payload (DownloadClientJobsAndStatus,
// download-client-data.ts:4-9). ratioItems is only present when a selection
// was applied upstream.
type JobsAndStatus struct {
	Status     Status      `json:"status"`
	Items      []Item      `json:"items"`
	RatioItems []RatioItem `json:"ratioItems,omitempty"`
}

// Selection mirrors DownloadClientSelection
// (download-client-integration.ts:4-11), the widget-side sort/filter request.
type Selection struct {
	Sort                   string
	Descending             bool
	CategoryFilter         []string
	FilterIsWhitelist      bool
	ShowCompletedTorrent   bool
	ActiveTorrentThreshold float64
}

// JobsInput is the input of GetJobsAndStatus. historyWindowDays applies to
// SABnzbd only (default 7 upstream).
type JobsInput struct {
	Limit                  float64
	Selection              *Selection
	IncludeArchivedHistory bool
	HistoryWindowDays      int64
}

// Client is the Go mirror of IDownloadClientIntegration. Implementations must
// be safe for concurrent use. Errors are *Error values from this package so
// callers can distinguish transport failures from bad responses.
type Client interface {
	GetJobsAndStatus(ctx context.Context, input JobsInput) (JobsAndStatus, error)
	PauseQueue(ctx context.Context) error
	PauseItem(ctx context.Context, item Item) error
	ResumeQueue(ctx context.Context) error
	ResumeItem(ctx context.Context, item Item) error
	DeleteItem(ctx context.Context, item Item, fromDisk bool) error
}

// Re-export the shared error taxonomy of the integrations domain so callers
// of both domains see one vocabulary (the upstream classes are shared too).
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
// it to the client factories (mirrors mediaorganizer.IntegrationRef).
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

// SelectTorrents ports selectDownloadTorrents
// (packages/integrations/src/download-client/select-torrents.ts:8-50): the
// local filter/sort/window applied by the providers that fetch their full RPC
// response (transmission, deluge).
//
// fields extracts the comparable value per sort field; a nil extractor or a
// nil value sorts as 0 upstream (`sortValue(left) ?? 0`). "time" treats 0 as
// Infinity (no eta). Strings compare with localeCompare upstream — Go uses
// plain byte comparison (documented deviation, same as the bangs port).
func SelectTorrents[T any](torrents []T, input JobsInput, fields Fields[T]) []T {
	limit := int(input.Limit) // JS slice truncates toward zero
	if input.Selection == nil {
		return sliceWindow(torrents, limit)
	}
	selection := input.Selection

	eligible := make([]T, 0, len(torrents))
	for _, torrent := range torrents {
		if len(selection.CategoryFilter) > 0 {
			category := fields.Category(torrent)
			var categories []string
			switch c := category.(type) {
			case string:
				categories = []string{c}
			case []string:
				categories = c
			default:
				// Upstream: fields.category(torrent) ?? "" -> [""].
				categories = []string{""}
			}
			matches := false
			for _, filter := range selection.CategoryFilter {
				if contains(categories, filter) {
					matches = true
					break
				}
			}
			if matches != selection.FilterIsWhitelist {
				continue
			}
		}
		progress := fields.Progress(torrent)
		if progress < 1 {
			eligible = append(eligible, torrent)
			continue
		}
		// Completed torrents stay only when shown AND actively seeding over
		// the threshold (threshold is KiB/s upstream -> *1024).
		if !selection.ShowCompletedTorrent {
			continue
		}
		if orZero(fields.UpSpeed(torrent)) < selection.ActiveTorrentThreshold*1024 {
			continue
		}
		eligible = append(eligible, torrent)
	}

	if extract, ok := fields.Sort[selection.Sort]; ok && extract != nil {
		multiplier := 1.0
		if selection.Descending {
			multiplier = -1
		}
		sort.SliceStable(eligible, func(left, right int) bool {
			lv, rv := extract(eligible[left]), extract(eligible[right])
			if selection.Sort == "time" {
				// Upstream maps a 0 eta to Infinity before comparing
				// (select-torrents.ts:35-38).
				if lv == 0 {
					lv = math.Inf(1)
				}
				if rv == 0 {
					rv = math.Inf(1)
				}
			}
			if ls, lok := lv.(string); lok {
				rs, rok := rv.(string)
				if !rok {
					return false
				}
				return strings.Compare(ls, rs)*int(multiplier) < 0
			}
			ln, lok := lv.(float64)
			rn, rok := rv.(float64)
			if !lok || !rok {
				return false
			}
			return (ln-rn)*multiplier < 0
		})
	}
	return sliceWindow(eligible, limit)
}

func sliceWindow[T any](items []T, limit int) []T {
	if limit < 0 {
		limit = 0
	}
	if limit > len(items) {
		limit = len(items)
	}
	return items[:limit]
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func orZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// Fields extracts the comparable values of one torrent for SelectTorrents.
// Category/Progress/UpSpeed are required upstream (TorrentSelectionFields);
// everything else is optional and defaults to 0. Numeric extractors return
// float64, string extractors string, categories string or []string.
type Fields[T any] struct {
	Category func(T) any
	Progress func(T) float64
	UpSpeed  func(T) *float64
	Sort     map[string]func(T) any
}
