package board

import "github.com/ydtoloza/homarr/server/internal/db"

// Domain constants ported from @homarr/definitions.

// widgetKinds mirrors widgetKinds (packages/definitions/src/widget.ts:1-69):
// the accepted "kind" of board items.
var widgetKinds = []string{
	"stats",
	"clock",
	"weather",
	"airQuality",
	"countdown",
	"timer",
	"app",
	"iframe",
	"video",
	"notebook",
	"anchorNote",
	"dnsHoleSummary",
	"dnsHoleControls",
	"smartHome-entityState",
	"smartHome-executeAutomation",
	"stockPrice",
	"mediaServer",
	"calendar",
	"downloads",
	"mediaRequests-requestList",
	"mediaRequests-requestStats",
	"mediaTranscoding",
	"mediaMissing",
	"minecraftServerStatus",
	"networkControllerSummary",
	"networkControllerStatus",
	"rssFeed",
	"bookmarks",
	"indexerManager",
	"healthMonitoring",
	"releases",
	"mediaReleases",
	"dockerContainers",
	"firewall",
	"notifications",
	"systemResources",
	"coolify",
	"systemDisks",
	"timetable",
	"immich-serverStats",
	"immich-albumCarousel",
	"paperlessNgx",
	"thingport-libraryStats",
	"thingport-models",
	"thingport-featuredModel",
	"thingport-activityHeatmap",
	"patchmon",
	"bazarr",
	"tracearr",
	"speedtestTracker",
	"uptimeKuma",
	"audioStats",
	"umami",
	"vpn",
	"archiveTeamWarrior",
	"ups",
	"beszelSystemTable",
	"beszelSystemGrid",
	"beszelAlerts",
	"beszelSystemStats",
	"traefik",
	"customApi",
	"assistant",
	"wud",
	"llamacpp",
	"wazuh",
}

// widgetKindSet is the lookup form of widgetKinds.
var widgetKindSet = func() map[string]bool {
	set := make(map[string]bool, len(widgetKinds))
	for _, kind := range widgetKinds {
		set[kind] = true
	}
	return set
}()

// widgetDefaultSizes mirrors widgetDefaultSizes
// (packages/definitions/src/widget.ts:72-98); kinds missing from the map fall
// back to {width: 1, height: 1} in addItem
// (packages/api/src/router/board.ts:2339).
var widgetDefaultSizes = map[string][2]int64{
	"clock":                      {2, 1},
	"weather":                    {2, 1},
	"airQuality":                 {2, 1},
	"calendar":                   {2, 2},
	"countdown":                  {2, 1},
	"customApi":                  {3, 2},
	"downloads":                  {4, 2},
	"indexerManager":             {2, 3},
	"timer":                      {2, 1},
	"bookmarks":                  {2, 2},
	"uptimeKuma":                 {2, 3},
	"audioStats":                 {2, 2},
	"paperlessNgx":               {2, 2},
	"thingport-libraryStats":     {2, 2},
	"thingport-models":           {3, 3},
	"thingport-featuredModel":    {2, 2},
	"thingport-activityHeatmap":  {6, 2},
	"patchmon":                   {2, 2},
	"mediaServer":                {3, 2},
	"mediaRequests-requestList":  {3, 2},
	"mediaRequests-requestStats": {2, 2},
	"mediaMissing":               {2, 2},
	"bazarr":                     {2, 2},
	"assistant":                  {10, 4},
	"wazuh":                      {4, 3},
}

// boardLanes and the root section offsets mirror section.ts of
// @homarr/definitions: root "empty" sections sit at xOffset -1 (left), 0
// (main) and 1 (right).
const (
	laneLeft  = "left"
	laneMain  = "main"
	laneRight = "right"
)

// boardLanes mirrors boardLanes (packages/definitions/src/section.ts:6): the
// fixed left → main → right iteration order of the lane math.
var boardLanes = []string{laneLeft, laneMain, laneRight}

const (
	rootSectionOffsetLeft  = -1
	rootSectionOffsetMain  = 0
	rootSectionOffsetRight = 1
)

// getRootSectionLane mirrors getRootSectionLane
// (packages/definitions/src/section.ts:15-20): only the exact root offsets
// are lanes; anything else belongs to the main canvas.
func getRootSectionLane(xOffset int64) string {
	switch xOffset {
	case rootSectionOffsetLeft:
		return laneLeft
	case rootSectionOffsetRight:
		return laneRight
	default:
		return laneMain
	}
}

// layout is the geometry subset of db.Layout used by the lane math.
type layoutGeometry struct {
	role                   string
	columnCount            int64
	leftGutterColumnCount  int64
	rightGutterColumnCount int64
}

func layoutGeometryOf(l db.Layout) layoutGeometry {
	return layoutGeometry{
		role:                   l.Role,
		columnCount:            l.ColumnCount,
		leftGutterColumnCount:  l.LeftGutterColumnCount,
		rightGutterColumnCount: l.RightGutterColumnCount,
	}
}

// getBoardLaneColumnCount mirrors getBoardLaneColumnCount
// (packages/definitions/src/section.ts:21-41): the number of columns a lane
// occupies inside one layout.
func getBoardLaneColumnCount(g layoutGeometry, lane string) int64 {
	total := g.columnCount
	if total < 1 {
		total = 1
	}
	left, right := int64(0), int64(0)
	if g.role != "mobile" {
		left = g.leftGutterColumnCount
		if left < 0 {
			left = 0
		}
		if left > total-1 {
			left = total - 1
		}
		right = g.rightGutterColumnCount
		if right < 0 {
			right = 0
		}
		if right > total-left-1 {
			right = total - left - 1
		}
	}
	switch lane {
	case laneLeft:
		return left
	case laneRight:
		return right
	default:
		return total - left - right
	}
}

// Enum value sets of the board settings (packages/definitions/src/board.ts).
var (
	backgroundImageAttachments = []string{"fixed", "scroll"}
	backgroundImageRepeats     = []string{"repeat", "repeat-x", "repeat-y", "no-repeat"}
	backgroundImageSizes       = []string{"cover", "contain"}
	cornerStyleValues          = []string{"xs", "sm", "md", "lg", "xl"}
	boardPermissionValues      = []string{"view", "modify", "full"}
	layoutRoles                = []string{"mobile", "base", "custom"}
)

// hexColorPrefix validates the #RRGGBB colors of the settings schemas
// (hexColorSchema, packages/validation/src/board.ts:17).
func isHexColor(v string) bool {
	if len(v) != 7 || v[0] != '#' {
		return false
	}
	for i := 1; i < len(v); i++ {
		c := v[i]
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// isBoardName validates boardNameSchema
// (packages/validation/src/board.ts:25-29): 1-255 chars of
// [A-Za-z0-9-_].
func isBoardName(name string) bool {
	if len(name) < 1 || len(name) > 255 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}
