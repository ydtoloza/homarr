package docker

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Container icon resolution — the port of resolveContainerIcon
// (packages/docker/src/discovery/parse-container-labels.ts:28-43) and the
// icon-table best match of getContainersWithStatsAsync
// (packages/request-handler/src/docker.ts:438-445, 500-503).

// docker and homepage label keys (packages/docker/src/labels.ts:1-21).
const (
	labelIcon  = "homarr.icon"
	labelName  = "homarr.name"
	homeIcon   = "homepage.icon"
	homePrefix = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/"
)

// dashboardIconRe is the Homepage Dashboard-Icons key of resolveIcon
// (parse-container-labels.ts:36): /^([a-zA-Z0-9][a-zA-Z0-9_-]*)(?:\.(png|svg|webp))?$/.
var dashboardIconRe = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9_-]*)(?:\.(png|svg|webp))?$`)

// iconifyRe is /^(mdi|si|sh)-/ (parse-container-labels.ts:33): iconify names
// pass through raw.
var iconifyRe = regexp.MustCompile(`^(mdi|si|sh)-`)

// resolveContainerIcon resolves the icon URL from the container labels: the
// homarr.icon label wins; the homepage.icon label applies only when no
// homarr.name label is set (useHomepageFallback), with the Dashboard-Icons
// PNG-default mapping. nil = no label icon.
func resolveContainerIcon(labels map[string]string) *string {
	homarrIcon := readLabel(labels, labelIcon)
	useHomepageFallback := readLabel(labels, labelName) == ""
	if homarrIcon != "" || !useHomepageFallback {
		if homarrIcon == "" {
			return nil
		}
		return &homarrIcon
	}

	homepageIcon := readLabel(labels, homeIcon)
	if homepageIcon == "" || iconifyRe.MatchString(homepageIcon) {
		if homepageIcon == "" {
			return nil
		}
		return &homepageIcon
	}

	// Homepage's Dashboard Icons keys default to PNG; URLs and local paths
	// pass through (parse-container-labels.ts:35-40).
	match := dashboardIconRe.FindStringSubmatch(homepageIcon)
	if match == nil || match[1] == "" {
		return &homepageIcon
	}
	extension := match[2]
	if extension == "" {
		extension = "png"
	}
	url := fmt.Sprintf("%s%s/%s.%s", homePrefix, extension, strings.ToLower(match[1]), extension)
	return &url
}

// readLabel is the trimmed non-empty label read of parse-container-labels.ts:16-19.
func readLabel(labels map[string]string, key string) string {
	return strings.TrimSpace(labels[key])
}

type iconRow struct {
	name string
	url  string
}

// loadIconCandidates queries the icon table for each distinct container
// image name (upstream: one findMany with OR of LIKE queries over the icons
// table, request-handler/src/docker.ts:438-445).
func (s *service) loadIconCandidates(ctx context.Context, infos []listedContainer) (map[string][]iconRow, *trpc.Error) {
	distinct := map[string]bool{}
	for _, lc := range infos {
		if name := extractImageName(lc.info.Image); name != "" {
			distinct[name] = true
		}
	}
	out := make(map[string][]iconRow, len(distinct))
	for name := range distinct {
		rows, err := s.db.QueryContext(ctx, `SELECT name, url FROM "icon" WHERE name LIKE ?`, "%"+name+"%")
		if err != nil {
			return nil, trpc.Internal(fmt.Errorf("docker: icon lookup for %q: %w", name, err))
		}
		candidates := []iconRow{}
		for rows.Next() {
			var row iconRow
			if err := rows.Scan(&row.name, &row.url); err != nil {
				rows.Close()
				return nil, trpc.Internal(fmt.Errorf("docker: scan icon row: %w", err))
			}
			candidates = append(candidates, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, trpc.Internal(fmt.Errorf("docker: icon lookup: %w", err))
		}
		rows.Close()
		out[name] = candidates
	}
	return out, nil
}

// extractImageName delegates to the docker package port of
// extractContainerImageName
// (packages/definitions/src/docker-integration-match.ts:6).
func extractImageName(image string) string {
	return docker.ExtractContainerImageName(image)
}

// bestMatch is the port of @homarr/common bestMatch
// (packages/common/src/string.ts:9-34): the icon whose name contains the
// search string at the earliest index wins; ties prefer the shorter name.
// Returns "" when nothing matches.
func bestMatch(candidates []iconRow, search string) string {
	if len(candidates) == 0 {
		return ""
	}
	normalized := strings.ToLower(strings.TrimSpace(search))
	bestIndex, bestLength := -1, -1
	best := ""
	found := false
	for _, candidate := range candidates {
		value := strings.ToLower(candidate.name)
		idx := strings.Index(value, normalized)
		if idx == -1 {
			continue
		}
		if !found || idx < bestIndex || (idx == bestIndex && len(value) < bestLength) {
			best = candidate.url
			bestIndex = idx
			bestLength = len(value)
			found = true
		}
	}
	return best
}
