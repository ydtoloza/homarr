package board

import (
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/db"
)

// Oracle tests ported from packages/api/src/router/board/test/
// grid-algorithm.spec.ts: every case of the upstream spec (grid text
// fixtures, nested container projections and the corrupted recursive section
// graph) plus extra cases for the preserved quirks.

const rootSectionID = "section"

// gridFixtureItem is algoItem (grid-algorithm.spec.ts:103-112): the spec
// defaults with explicit values instead of generated cuids.
func gridFixtureItem(id string, itemType string, width, height, xOffset, yOffset int64, sectionID string) gridItem {
	return gridItem{
		ID: id, Type: itemType,
		Width: width, Height: height,
		XOffset: xOffset, YOffset: yOffset,
		SectionID: sectionID,
	}
}

// generateInputFromText ports generateInputFromText
// (grid-algorithm.spec.ts:315-339): a text grid becomes one item per
// connected letter block. Runes (not bytes) are required by the ä/ö fixtures.
func generateInputFromText(text string) []gridItem {
	lines := strings.Split(text, "\n")[1:] // Remove first empty row
	runes := make([][]rune, len(lines))
	for i, line := range lines {
		runes[i] = []rune(line)
	}
	var items []gridItem
	for yOffset, line := range runes {
		for xOffset := 0; xOffset < len(line); xOffset++ {
			char := line[xOffset]
			if char == ' ' {
				continue
			}
			known := false
			for _, item := range items {
				if item.ID == string(char) {
					known = true
					break
				}
			}
			if known {
				continue
			}
			items = append(items, gridItem{
				ID:      string(char),
				Type:    "item",
				Width:   fixtureGetWidth(line, xOffset, char),
				Height:  fixtureGetHeight(runes, xOffset, yOffset, char),
				XOffset: int64(xOffset),
				YOffset: int64(yOffset),
				// Every fixture item lives on the root canvas.
				SectionID: rootSectionID,
			})
		}
	}
	return items
}

// fixtureGetWidth ports getWidth (grid-algorithm.spec.ts:350-361).
func fixtureGetWidth(line []rune, offset int, char rune) int64 {
	width := int64(1)
	for xOffset := offset + 1; xOffset < len(line); xOffset++ {
		if line[xOffset] == char {
			width++
		} else {
			break
		}
	}
	return width
}

// fixtureGetHeight ports getHeight (grid-algorithm.spec.ts:363-373).
func fixtureGetHeight(lines [][]rune, x, y int, char rune) int64 {
	height := int64(1)
	for yOffset := y + 1; yOffset < len(lines); yOffset++ {
		if x < len(lines[yOffset]) && lines[yOffset][x] == char {
			height++
		} else {
			break
		}
	}
	return height
}

// generateOutputText ports generateOutputText (grid-algorithm.spec.ts:341-348):
// the items painted onto a fresh grid, one row per occupied line.
func generateOutputText(items []gridItem, columnCount int64) string {
	var occupied [][]rune
	for _, item := range items {
		for yOffset := int64(0); yOffset < item.Height; yOffset++ {
			row := item.YOffset + yOffset
			for int64(len(occupied)) <= row {
				row2 := make([]rune, columnCount)
				for i := range row2 {
					row2[i] = ' '
				}
				occupied = append(occupied, row2)
			}
			for xOffset := int64(0); xOffset < item.Width; xOffset++ {
				occupied[row][item.XOffset+xOffset] = []rune(item.ID)[0]
			}
		}
	}
	rows := make([]string, len(occupied))
	for i, row := range occupied {
		rows[i] = string(row)
	}
	return "\n" + strings.Join(rows, "\n")
}

// --- upstream fixtures (grid-algorithm.spec.ts:114-313) -----------------------

const sixteenColumns = `
abbccccddddeeefg
hbbccccddddeeeij
klllmmmmmnneeeop
qlllmmmmmnnrrrst
ulllmmmmmnnrrrvw
xyz           äö`

// eighteenColumnsText is eighteenColumns (grid-algorithm.spec.ts:123-126):
// just add two empty columns to the right.
func eighteenColumnsText() string {
	lines := strings.Split(sixteenColumns, "\n")
	out := make([]string, len(lines))
	for index, line := range lines {
		if index == 0 {
			out[index] = line
			continue
		}
		out[index] = line + "  "
	}
	return strings.Join(out, "\n")
}

const tenColumns = `
abbcccceee
fbbcccceee
ddddghieee
ddddjklllo
mmmmmplllq
mmmmmslllt
mmmmmnnrrr
uvwxynnrrr
zäö  nn   `

const sixColumns = `
abbfgh
ibbjko
ccccnn
ccccnn
ddddnn
ddddpq
eeelll
eeelll
eeelll
mmmmms
mmmmmt
mmmmmu
rrrvwx
rrryzä
ö     `

const threeColumns = `
abb
fbb
ccc
ccc
ddd
ddd
eee
eee
eee
ghi
jko
lll
lll
lll
mmm
mmm
mmm
nnp
nnq
nns
rrr
rrr
tuv
wxy
zäö`

func TestGridAlgorithmItemFixtures(t *testing.T) {
	tests := []struct {
		name              string
		input             string
		inputColumnCount  int64
		output            string
		outputColumnCount int64
	}{
		{"16 to 16", sixteenColumns, 16, sixteenColumns, 16},
		{"16 to 18", sixteenColumns, 16, eighteenColumnsText(), 18},
		{"16 to 10", sixteenColumns, 16, tenColumns, 10},
		{"16 to 6", sixteenColumns, 16, sixColumns, 6},
		{"16 to 3", sixteenColumns, 16, threeColumns, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := generateInputFromText(tt.input)

			result, err := generateResponsiveGridFor(input, tt.outputColumnCount, tt.inputColumnCount, rootSectionID)
			if err != nil {
				t.Fatalf("generateResponsiveGridFor: %v", err)
			}

			output := generateOutputText(result.Items, tt.outputColumnCount)
			if output != tt.output {
				t.Fatalf("output mismatch:\n got:\n%s\nwant:\n%s", output, tt.output)
			}
		})
	}
}

// containerFixture builds the nested container fixture of
// grid-algorithm.spec.ts:28-80 (ids a-i with the outer container "b" and the
// inner container "f").
func containerFixture() []gridItem {
	const outerContainerID = "b"
	const innerContainerID = "f"
	return []gridItem{
		gridFixtureItem("a", "item", 2, 2, 0, 0, rootSectionID),
		gridFixtureItem(outerContainerID, "section", 12, 3, 0, 2, rootSectionID),
		gridFixtureItem("a", "item", 2, 1, 0, 0, outerContainerID),
		gridFixtureItem("b", "item", 4, 1, 2, 0, outerContainerID),
		gridFixtureItem("c", "item", 2, 1, 6, 0, outerContainerID),
		gridFixtureItem("d", "item", 1, 1, 8, 0, outerContainerID),
		gridFixtureItem("e", "item", 3, 1, 9, 0, outerContainerID),
		gridFixtureItem(innerContainerID, "section", 8, 2, 0, 1, outerContainerID),
		gridFixtureItem("a", "item", 2, 1, 0, 0, innerContainerID),
		gridFixtureItem("b", "item", 5, 1, 2, 0, innerContainerID),
		gridFixtureItem("c", "item", 1, 2, 7, 0, innerContainerID),
		gridFixtureItem("d", "item", 7, 1, 0, 1, innerContainerID),
		gridFixtureItem("g", "item", 4, 1, 8, 1, outerContainerID),
		gridFixtureItem("h", "item", 3, 1, 8, 2, outerContainerID),
		gridFixtureItem("i", "item", 1, 1, 11, 2, outerContainerID),
		gridFixtureItem("c", "item", 5, 1, 0, 5, rootSectionID),
	}
}

// padGrid right-pads every line with spaces to the given column count. The
// upstream spec embeds the padding literally in its expected strings
// (generateOutputText always renders rows of exactly columnCount runes);
// padding programmatically keeps the Go tables readable. The leading empty
// line (the artifact of the template literal) stays empty, like upstream.
func padGrid(text string, columns int64) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if i == 0 {
			continue
		}
		runes := []rune(line)
		for int64(len(runes)) < columns {
			runes = append(runes, ' ')
		}
		lines[i] = string(runes)
	}
	return strings.Join(lines, "\n")
}

func TestGridAlgorithmContainerFixtures(t *testing.T) {
	tests := []struct {
		name          string
		outputColumns int64
		root          string
		outer         string
		inner         string
	}{
		{
			name:          "16 columns",
			outputColumns: 16,
			root: `
aa
aa
bbbbbbbbbbbb
bbbbbbbbbbbb
bbbbbbbbbbbb
ccccc`,
			outer: `
aabbbbccdeee
ffffffffgggg
ffffffffhhhi`,
			inner: `
aabbbbbc
dddddddc`,
		},
		{
			name:          "10 columns",
			outputColumns: 10,
			root: `
aaccccc
aa
bbbbbbbbbb
bbbbbbbbbb
bbbbbbbbbb
bbbbbbbbbb`,
			outer: `
aabbbbccdi
eeegggghhh
ffffffff
ffffffff`,
			inner: `
aabbbbbc
dddddddc`,
		},
		{
			name:          "6 columns",
			outputColumns: 6,
			root: `
aa
aa
bbbbbb
bbbbbb
bbbbbb
bbbbbb
bbbbbb
bbbbbb
bbbbbb
ccccc`,
			outer: `
aabbbb
ccdeee
ffffff
ffffff
ffffff
ggggi
hhh`,
			inner: `
aa   c
bbbbbc
dddddd`,
		},
		{
			name:          "3 columns",
			outputColumns: 3,
			root: `
aa
aa
bbb
bbb
bbb
bbb
bbb
bbb
bbb
bbb
bbb
bbb
bbb
ccc`,
			outer: `
aad
bbb
cci
eee
fff
fff
fff
fff
fff
ggg
hhh`,
			inner: `
aa
bbb
c
c
ddd`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := containerFixture()

			result, err := generateResponsiveGridFor(items, tt.outputColumns, 16, rootSectionID)
			if err != nil {
				t.Fatalf("generateResponsiveGridFor: %v", err)
			}

			var rootItems, outerItems, innerItems []gridItem
			for _, item := range result.Items {
				switch item.SectionID {
				case rootSectionID:
					rootItems = append(rootItems, item)
				case "b":
					outerItems = append(outerItems, item)
				case "f":
					innerItems = append(innerItems, item)
				}
			}
			// The spec renders every section at min(outputColumns, width of the
			// section placement in the input fixture).
			if got := generateOutputText(rootItems, tt.outputColumns); got != padGrid(tt.root, tt.outputColumns) {
				t.Fatalf("root mismatch:\n got:\n%s\nwant:\n%s", got, padGrid(tt.root, tt.outputColumns))
			}
			if got := generateOutputText(outerItems, min(tt.outputColumns, 12)); got != padGrid(tt.outer, min(tt.outputColumns, 12)) {
				t.Fatalf("outer mismatch:\n got:\n%s\nwant:\n%s", got, padGrid(tt.outer, min(tt.outputColumns, 12)))
			}
			if got := generateOutputText(innerItems, min(tt.outputColumns, 8)); got != padGrid(tt.inner, min(tt.outputColumns, 8)) {
				t.Fatalf("inner mismatch:\n got:\n%s\nwant:\n%s", got, padGrid(tt.inner, min(tt.outputColumns, 8)))
			}
		})
	}
}

// TestGridAlgorithmStopsRecursiveSectionGraph ports the corrupted graph test
// (grid-algorithm.spec.ts:82-100).
func TestGridAlgorithmStopsRecursiveSectionGraph(t *testing.T) {
	recursive := gridFixtureItem("recursive", "section", 3, 1, 0, 0, rootSectionID)
	duplicateRecursiveLayout := gridFixtureItem("recursive", "section", 3, 1, 0, 0, "recursive")

	result, err := generateResponsiveGridFor([]gridItem{recursive, duplicateRecursiveLayout}, 3, 12, rootSectionID)
	if err != nil {
		t.Fatalf("generateResponsiveGridFor: %v", err)
	}

	found := false
	for _, item := range result.Items {
		if item.ID == "recursive" && item.SectionID == rootSectionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("root recursive section missing: %+v", result.Items)
	}
	if len(result.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(result.Items))
	}
}

// --- extra cases for preserved quirks -----------------------------------------

// A widening projection keeps every offset and only clamps widths (the
// width >= previousWidth branch of grid-algorithm.ts:76-81).
func TestGridAlgorithmWideningKeepsOffsetsAndClampsWidth(t *testing.T) {
	items := []gridItem{
		gridFixtureItem("wide", "item", 6, 2, 2, 3, rootSectionID),
		gridFixtureItem("tall", "item", 1, 5, 0, 0, rootSectionID),
	}
	result, err := generateResponsiveGridFor(items, 4, 3, rootSectionID)
	if err != nil {
		t.Fatalf("generateResponsiveGridFor: %v", err)
	}
	if result.Height != 5 {
		t.Fatalf("height = %d, want 5 (declared placements)", result.Height)
	}
	byID := map[string]gridItem{}
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	if got := byID["wide"]; got.XOffset != 2 || got.YOffset != 3 || got.Height != 2 || got.Width != 4 {
		t.Fatalf("wide = %+v, want offsets kept and width clamped to 4", got)
	}
	if got := byID["tall"]; got.XOffset != 0 || got.YOffset != 0 || got.Width != 1 || got.Height != 5 {
		t.Fatalf("tall = %+v, want unchanged", got)
	}
}

// Narrowing packs in (yOffset, xOffset) order of the original placements,
// ignoring the original offsets afterwards.
func TestGridAlgorithmNarrowingPacksInScanOrder(t *testing.T) {
	items := []gridItem{
		// "late" has the larger original yOffset but is placed first in the
		// packed grid because its original offset sorts earlier... no: it
		// sorts LATER. The fixture asserts the (y, x) order effect.
		gridFixtureItem("late", "item", 2, 1, 4, 0, rootSectionID),
		gridFixtureItem("early", "item", 2, 1, 0, 1, rootSectionID),
	}
	result, err := generateResponsiveGridFor(items, 3, 6, rootSectionID)
	if err != nil {
		t.Fatalf("generateResponsiveGridFor: %v", err)
	}
	byID := map[string]gridItem{}
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	// late (y=0) is packed first: (0,0). early (y=1) cannot share row 0
	// (2+2 > 3 at the only remaining x), so it lands at (0,1).
	if got := byID["late"]; got.XOffset != 0 || got.YOffset != 0 {
		t.Fatalf("late = %+v, want (0,0)", got)
	}
	if got := byID["early"]; got.XOffset != 0 || got.YOffset != 1 {
		t.Fatalf("early = %+v, want (0,1)", got)
	}
	if result.Height != 2 {
		t.Fatalf("height = %d, want 2", result.Height)
	}
}

// A nested container shrinks (and its content reflows) even when the root
// projection itself widens, because the recursion runs before the
// width >= previousWidth early return (grid-algorithm.ts:61-74).
func TestGridAlgorithmNestedContainerReflowsWhileRootWidens(t *testing.T) {
	items := []gridItem{
		gridFixtureItem("box", "section", 12, 2, 0, 0, rootSectionID),
		gridFixtureItem("a", "item", 2, 1, 0, 0, "box"),
		gridFixtureItem("b", "item", 8, 1, 2, 0, "box"),
		gridFixtureItem("c", "item", 2, 1, 10, 0, "box"),
	}
	result, err := generateResponsiveGridFor(items, 10, 4, rootSectionID)
	if err != nil {
		t.Fatalf("generateResponsiveGridFor: %v", err)
	}
	byID := map[string]gridItem{}
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	// Root widened 4 -> 10: the container keeps its root placement and its
	// declared height (2), not the projected content height (1).
	if got := byID["box"]; got.XOffset != 0 || got.YOffset != 0 || got.Width != 10 || got.Height != 2 {
		t.Fatalf("box = %+v, want width clamped to 10 and declared height 2", got)
	}
	// The content reflowed from 12 columns to the clamped 10: a (0,0),
	// b (2,0), c (10,0) -> c no longer fits (10+2 > 10), it packs to (0,1).
	if got := byID["c"]; got.XOffset != 0 || got.YOffset != 1 {
		t.Fatalf("c = %+v, want packed to (0,1)", got)
	}
}

// In the packing branch the container height is the projected content height
// when it exceeds the declared height (grid-algorithm.ts:87).
func TestGridAlgorithmPackingUsesProjectedSectionHeight(t *testing.T) {
	items := []gridItem{
		gridFixtureItem("box", "section", 8, 1, 0, 0, rootSectionID),
		gridFixtureItem("a", "item", 2, 1, 0, 0, "box"),
		gridFixtureItem("b", "item", 2, 1, 2, 0, "box"),
		gridFixtureItem("c", "item", 2, 1, 0, 1, "box"),
		gridFixtureItem("d", "item", 2, 1, 2, 1, "box"),
		gridFixtureItem("e", "item", 4, 1, 0, 1, rootSectionID),
	}
	// 8 -> 4: the container content keeps its 2x2 arrangement, so the
	// container occupies height 2 despite being declared with height 1; the
	// root's own item packs below it.
	result, err := generateResponsiveGridFor(items, 4, 8, rootSectionID)
	if err != nil {
		t.Fatalf("generateResponsiveGridFor: %v", err)
	}
	byID := map[string]gridItem{}
	for _, item := range result.Items {
		byID[item.ID] = item
	}
	if got := byID["box"]; got.Height != 2 || got.XOffset != 0 || got.YOffset != 0 {
		t.Fatalf("box = %+v, want height 2 at (0,0)", got)
	}
	if got := byID["e"]; got.XOffset != 0 || got.YOffset != 2 {
		t.Fatalf("e = %+v, want packed below the box", got)
	}
	if result.Height != 3 {
		t.Fatalf("root height = %d, want 3", result.Height)
	}
}

// A section without placements projects to an empty result
// (grid-algorithm.ts:58).
func TestGridAlgorithmEmptySection(t *testing.T) {
	result, err := generateResponsiveGridFor([]gridItem{
		gridFixtureItem("a", "item", 2, 1, 0, 0, rootSectionID),
	}, 4, 2, rootSectionID)
	if err != nil {
		t.Fatalf("generateResponsiveGridFor: %v", err)
	}
	if result.Height != 1 || len(result.Items) != 1 {
		t.Fatalf("root = %+v, want one item", result)
	}
	empty, err := generateResponsiveGridFor(nil, 4, 2, rootSectionID)
	if err != nil {
		t.Fatalf("generateResponsiveGridFor: %v", err)
	}
	if empty.Height != 0 || len(empty.Items) != 0 {
		t.Fatalf("empty = %+v, want height 0 and no items", empty)
	}
}

// The projection input must attach the container placements to every section
// (not only the last one appended): sections are indexed, never re-pointed.
func TestProjectionInputAttachesLayoutsToAllSections(t *testing.T) {
	sections := []db.Section{
		{ID: "root", Kind: "empty", XOffset: nullInt64(0), YOffset: nullInt64(0)},
		{ID: "c1", Kind: "container"},
		{ID: "c2", Kind: "container"},
	}
	sectionLayouts := []db.SectionLayout{
		{SectionID: "c1", LayoutID: "layout-1", ParentSectionID: "root", Width: 3, Height: 2},
		{SectionID: "c2", LayoutID: "layout-1", ParentSectionID: "root", Width: 4, Height: 1},
	}
	items := []db.Item{
		{ID: "item-1", BoardID: "b", Kind: "clock", Options: `{"json": {}}`, AdvancedOptions: `{"json": {}}`},
	}
	itemLayouts := []db.ItemLayout{
		{ItemID: "item-1", SectionID: "root", LayoutID: "layout-1", Width: 2, Height: 1},
	}

	board := newBoardForLayoutProjection("b", nil, sections, sectionLayouts, items, itemLayouts)
	if len(board.Sections) != 3 {
		t.Fatalf("sections = %d, want 3", len(board.Sections))
	}
	if root := board.Sections[0]; root.Kind != "empty" || root.XOffset != 0 {
		t.Fatalf("root = %+v", root)
	}
	if got := board.Sections[1]; len(got.Layouts) != 1 || got.Layouts[0].ParentSectionID != "root" || got.Layouts[0].Width != 3 {
		t.Fatalf("c1 layouts = %+v", got.Layouts)
	}
	if got := board.Sections[2]; len(got.Layouts) != 1 || got.Layouts[0].Width != 4 {
		t.Fatalf("c2 layouts = %+v", got.Layouts)
	}
	if len(board.Items) != 1 || len(board.Items[0].Layouts) != 1 || board.Items[0].Layouts[0].SectionID != "root" {
		t.Fatalf("items = %+v", board.Items)
	}
}
