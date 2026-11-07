package appmeta

// The website metadata detection — the port of fetchWebsiteMetadataAsync
// (packages/icons/src/favicon-fetcher.ts): resolve the title and icon of a
// website by reading the <title> and the <link rel="icon"> declarations of
// the page it serves, falling back to the well known /favicon.ico of the
// origin that actually answered. Every request goes through the guarded
// client (guardedfetch.go). Either value stays empty (null) when it cannot
// be determined; the detection never fails (fetchPageAsync and isImageAsync
// swallow their errors, favicon-fetcher.ts:99-119, 238-254).

import (
	"context"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// requestTimeoutInMs / detectionTimeoutInMs / maximumHtmlBytes
	// (favicon-fetcher.ts:15-23): the page, one redirect and the favicon.ico
	// probe each use their full request timeout, while a server that drips
	// its page byte by byte cannot hold the detection open until the size
	// limit.
	requestTimeout   = 5 * time.Second
	detectionTimeout = 3 * requestTimeout
	maximumHTMLBytes = 256 * 1024
	// maximumTitleLength (favicon-fetcher.ts:171) — applied to the raw match
	// BEFORE the entity decode, like the upstream .slice(0, 512) (byte-wise
	// in Go where the upstream slice counts UTF-16 units; only a title cut
	// at a multi-byte character notices).
	maximumTitleLength = 512
)

// websiteMetadata is WebsiteMetadata (favicon-fetcher.ts:52-55): empty
// strings for the null values.
type websiteMetadata struct {
	Title   string
	IconURL string
}

// parseHTTPURL is parseHttpUrl (favicon-fetcher.ts:82-90): trim, http/https
// only, credentials and fragment stripped. nil when the value is not a
// usable website URL.
func parseHTTPURL(value string) *url.URL {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil
	}
	parsed.User = nil
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed
}

// fetchWebsiteMetadata is fetchWebsiteMetadataAsync
// (favicon-fetcher.ts:58-77).
func fetchWebsiteMetadata(ctx context.Context, client *guardedHTTP, href string) websiteMetadata {
	websiteURL := parseHTTPURL(href)
	if websiteURL == nil {
		return websiteMetadata{}
	}
	ctx, cancel := context.WithTimeout(ctx, detectionTimeout)
	defer cancel()
	return detectWebsiteMetadata(ctx, client, websiteURL)
}

// detectWebsiteMetadata is detectWebsiteMetadataAsync
// (favicon-fetcher.ts:79-97).
func detectWebsiteMetadata(ctx context.Context, client *guardedHTTP, websiteURL *url.URL) websiteMetadata {
	page := fetchPage(ctx, client, websiteURL)
	var title, declared string
	if page != nil && page.html != "" {
		title = findTitle(page.html)
		declared = findDeclaredIconURL(page.html, page.finalURL)
	}
	if declared != "" {
		return websiteMetadata{Title: title, IconURL: declared}
	}
	// The well known icon belongs to the origin that served the page, which
	// differs from the requested one when the app redirects across origins
	// (favicon-fetcher.ts:88-96).
	origin := websiteURL.Scheme + "://" + websiteURL.Host
	if page != nil {
		origin = page.finalURL.Scheme + "://" + page.finalURL.Host
	}
	wellKnown := origin + "/favicon.ico"
	if isImage(ctx, client, wellKnown) {
		return websiteMetadata{Title: title, IconURL: wellKnown}
	}
	return websiteMetadata{Title: title}
}

// fetchedPage is the {html, url} pair of fetchPageAsync; nil when no
// response was obtained at all.
type fetchedPage struct {
	html     string // empty = no usable html
	finalURL *url.URL
}

// fetchPage is fetchPageAsync (favicon-fetcher.ts:92-119): GET with
// Accept text/html; the html is read only when the response is ok and
// text/html; once a response exists, its (final) url is always returned so
// the well known fallback targets the origin that answered.
func fetchPage(ctx context.Context, client *guardedHTTP, websiteURL *url.URL) *fetchedPage {
	res, finalURL, err := client.get(ctx, websiteURL, map[string]string{"Accept": "text/html"}, requestTimeout)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	page := &fetchedPage{finalURL: finalURL}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return page
	}
	contentType := strings.ToLower(res.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/html") {
		return page
	}
	html, err := readDocumentHead(res.Body)
	if err != nil {
		return page // logged debug upstream; the html stays null
	}
	page.html = html
	return page
}

// headEndRegex matches </head with a following [\s/>] or <body with
// [\s/>] — the head also ends at <body> because </head> is optional in HTML
// and minifiers drop it (favicon-fetcher.ts:122-123). The character class
// keeps longer names such as </header> from matching.
var headEndRegex = regexp.MustCompile(`(?i)(</head[\s/>]|<body[\s/>])`)

// readDocumentHead is readDocumentHeadAsync (favicon-fetcher.ts:125-163):
// stream up to maximumHTMLBytes, stop at the head end. Only the new text and
// the few characters before it are searched, so a page sent in tiny chunks
// costs no more than one sent at once (a "</head" can straddle a chunk
// boundary).
func readDocumentHead(r io.Reader) (string, error) {
	var builder strings.Builder
	tail := ""
	buf := make([]byte, 8192)
	for builder.Len() < maximumHTMLBytes {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			searched := tail + chunk
			if loc := headEndRegex.FindStringIndex(searched); loc != nil {
				builder.WriteString(searched[:loc[0]])
				return builder.String(), nil
			}
			builder.WriteString(chunk)
			if len(searched) > len("</head") {
				tail = searched[len(searched)-len("</head"):]
			} else {
				tail = searched
			}
		}
		if err != nil {
			// EOF: the head never closed; everything read is the search
			// space (bounded by the byte cap).
			return builder.String(), nil
		}
	}
	return builder.String(), nil
}

// linkTagRegex / attributeRegex / sizeRegex / titleRegex / entityRegex
// mirror favicon-fetcher.ts:166-172. RE2 has no lookbehind, so the attribute
// and size matches post-filter the preceding byte (parseAttributes,
// parseLargestSizeArea).
var (
	linkTagRegex = regexp.MustCompile(`(?i)<link\b[^<>]*>`)
	attributeRe  = regexp.MustCompile(`(?i)([a-z-]+)\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	sizeRegex    = regexp.MustCompile(`(\d+)x(\d+)`)
	titleRegex   = regexp.MustCompile(`(?i)<title\b[^<>]*>([^<]*)`)
	entityRegex  = regexp.MustCompile(`&(#x[\da-f]{1,6}|#\d{1,7}|[a-z]{2,8});`)
)

var namedEntities = map[string]string{"amp": "&", "lt": "<", "gt": ">", "quot": `"`, "apos": "'", "nbsp": " "}

// findTitle is findTitle (favicon-fetcher.ts:174-184): the first title tag,
// entities decoded, whitespace collapsed; empty when the title is absent or
// blank.
func findTitle(html string) string {
	match := titleRegex.FindStringSubmatch(html)
	if match == nil {
		return ""
	}
	raw := match[1]
	if len(raw) > maximumTitleLength {
		raw = raw[:maximumTitleLength]
	}
	title := strings.Join(strings.Fields(decodeEntities(raw)), " ")
	return strings.TrimSpace(title)
}

// decodeEntities is decodeEntities (favicon-fetcher.ts:186-199): numeric and
// the five known named entities; unknown names stay verbatim.
func decodeEntities(text string) string {
	return entityRegex.ReplaceAllStringFunc(text, func(entity string) string {
		name := entity[1 : len(entity)-1]
		if !strings.HasPrefix(name, "#") {
			if decoded, ok := namedEntities[strings.ToLower(name)]; ok {
				return decoded
			}
			return entity
		}
		var codePoint int64
		var err error
		if len(name) > 1 && (name[1] == 'x' || name[1] == 'X') {
			codePoint, err = strconv.ParseInt(name[2:], 16, 32)
		} else {
			codePoint, err = strconv.ParseInt(name[1:], 10, 32)
		}
		if err != nil || codePoint <= 0 || codePoint > 0x10FFFF {
			return entity
		}
		return string(rune(codePoint))
	})
}

// iconRelationScores is iconRelationScores (favicon-fetcher.ts:25-30): the
// relation is the primary ranking signal because the sizes attribute is
// omitted far too often to compare candidates of different relations.
var iconRelationScores = map[string]int{
	"apple-touch-icon-precomposed": 4,
	"apple-touch-icon":             3,
	"icon":                         2,
	"shortcut icon":                1,
}

// iconCandidate is IconCandidate (favicon-fetcher.ts:32-37).
type iconCandidate struct {
	url           string
	relationScore int
	sizeArea      int
}

// findDeclaredIconURL is findDeclaredIconURL (favicon-fetcher.ts:201-235):
// the <link> candidates ranked by relation score, then by the largest size
// area; the first of the ranking wins.
func findDeclaredIconURL(html string, pageURL *url.URL) string {
	var candidates []iconCandidate
	for _, linkTag := range linkTagRegex.FindAllString(html, -1) {
		attributes := parseAttributes(linkTag)
		relation := strings.Join(strings.Fields(strings.ToLower(attributes["rel"])), " ")
		relationScore, ok := iconRelationScores[relation]
		if !ok {
			continue
		}
		href, ok := attributes["href"]
		if !ok {
			continue
		}
		resolved := resolveHTTPURL(href, pageURL)
		if resolved == "" {
			continue
		}
		candidates = append(candidates, iconCandidate{
			url:           resolved,
			relationScore: relationScore,
			sizeArea:      parseLargestSizeArea(attributes["sizes"]),
		})
	}
	// The toSorted comparator of favicon-fetcher.ts:231-233: relation score
	// descending, then size area descending; the first candidate wins.
	best := -1
	for i, c := range candidates {
		if best == -1 ||
			c.relationScore > candidates[best].relationScore ||
			(c.relationScore == candidates[best].relationScore && c.sizeArea > candidates[best].sizeArea) {
			best = i
		}
	}
	if best == -1 {
		return ""
	}
	return candidates[best].url
}

// parseAttributes is parseAttributes (favicon-fetcher.ts:237-250): name and
// value of every attribute, names lowercased. The upstream lookbehind
// ((?<![a-z-])) keeps a suffix of a longer name (data-icon → icon) from
// matching; the port checks the preceding byte instead.
func parseAttributes(linkTag string) map[string]string {
	attributes := map[string]string{}
	for _, loc := range attributeRe.FindAllStringSubmatchIndex(linkTag, -1) {
		if start := loc[0]; start > 0 {
			preceding := linkTag[start-1]
			if (preceding >= 'a' && preceding <= 'z') || (preceding >= 'A' && preceding <= 'Z') || preceding == '-' {
				continue
			}
		}
		name := strings.ToLower(linkTag[loc[2]:loc[3]])
		// Value: the first of the three value groups that participated
		// (group 2 is the whole alternative incl. quotes; 3/4/5 are the
		// double-quoted, single-quoted and bare contents).
		valueStart, valueEnd := -1, -1
		for _, pair := range [][2]int{{6, 7}, {8, 9}, {10, 11}} {
			if loc[pair[0]] != -1 {
				valueStart, valueEnd = loc[pair[0]], loc[pair[1]]
				break
			}
		}
		if valueStart == -1 {
			continue
		}
		attributes[name] = linkTag[valueStart:valueEnd]
	}
	return attributes
}

// parseLargestSizeArea is parseLargestSizeArea (favicon-fetcher.ts:252-263):
// the largest WxH area of a sizes attribute like "32x32 180x180". The
// upstream lookbehind ((?<!\d)) skips matches that start right after a
// digit; the port checks the preceding byte.
func parseLargestSizeArea(sizes string) int {
	if sizes == "" {
		return 0
	}
	largest := 0
	for _, loc := range sizeRegex.FindAllStringSubmatchIndex(sizes, -1) {
		if start := loc[0]; start > 0 && sizes[start-1] >= '0' && sizes[start-1] <= '9' {
			continue
		}
		w, _ := strconv.Atoi(sizes[loc[2]:loc[3]])
		h, _ := strconv.Atoi(sizes[loc[4]:loc[5]])
		if area := w * h; area > largest {
			largest = area
		}
	}
	return largest
}

// resolveHTTPURL is resolveHttpUrl (favicon-fetcher.ts:265-273): resolve
// against the page url; http/https only.
func resolveHTTPURL(href string, pageURL *url.URL) string {
	resolved, err := url.Parse(href)
	if err != nil {
		return ""
	}
	resolved = pageURL.ResolveReference(resolved)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return ""
	}
	return resolved.String()
}

// isImage is isImageAsync (favicon-fetcher.ts:275-295): the well known
// favicon probe — only the content type tells a real icon from the HTML
// entry point many self-hosted apps answer unknown paths with.
func isImage(ctx context.Context, client *guardedHTTP, rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	res, _, err := client.get(ctx, parsed, nil, requestTimeout)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return false
	}
	contentType := strings.ToLower(res.Header.Get("Content-Type"))
	return strings.HasPrefix(contentType, "image/") || strings.Contains(contentType, "icon")
}
