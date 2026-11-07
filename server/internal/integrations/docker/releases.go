package docker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// Release source tokens for the getContainers response: the port of
// getReleaseSourceFromDockerImage (packages/definitions/src/release-provider.ts:90-121)
// plus createKeyedFingerprint (packages/common/src/encryption.ts:36-38). The
// docker router attaches releaseSourceToken to every container whose image
// maps to a release provider, so the multi-releases widget can suggest the
// image as a release repository without leaking the raw image reference to
// other clients.

// ReleaseProviderKind is the ReleaseProviderKind subset reachable from a
// container image (containerImageToProviderKind, release-provider.ts:83-89).
type ReleaseProviderKind string

const (
	ReleaseProviderDockerHub               ReleaseProviderKind = "dockerHub"
	ReleaseProviderGitHubContainerRegistry ReleaseProviderKind = "gitHubContainerRegistry"
	ReleaseProviderLinuxServerIO           ReleaseProviderKind = "linuxServerIO"
	ReleaseProviderQuay                    ReleaseProviderKind = "quay"
)

// releaseProviderRegistryPrefixes (release-provider.ts:92-98). Every provider
// reachable from a container image carries a registry prefix.
var releaseProviderRegistryPrefixes = map[ReleaseProviderKind]string{
	ReleaseProviderDockerHub:               "docker.io/",
	ReleaseProviderGitHubContainerRegistry: "ghcr.io/",
	ReleaseProviderLinuxServerIO:           "lscr.io/",
	ReleaseProviderQuay:                    "quay.io/",
}

// containerImageToProviderKind (release-provider.ts:83-89).
var containerImageToProviderKind = map[string]ReleaseProviderKind{
	"ghcr.io":   ReleaseProviderGitHubContainerRegistry,
	"docker.io": ReleaseProviderDockerHub,
	"lscr.io":   ReleaseProviderLinuxServerIO,
	"quay.io":   ReleaseProviderQuay,
}

// ReleaseSource is the {provider, identifier, version?} result of
// getReleaseSourceFromDockerImage.
type ReleaseSource struct {
	Provider   ReleaseProviderKind
	Identifier string
	Version    string
}

// ExtractContainerImageName is extractContainerImageName
// (packages/definitions/src/docker-integration-match.ts:6): strip the
// registry and the tag ("ghcr.io/owner/app:v1" → "app").
func ExtractContainerImageName(image string) string {
	lastSegment := image
	if idx := strings.LastIndex(image, "/"); idx != -1 {
		lastSegment = image[idx+1:]
	}
	if idx := strings.Index(lastSegment, ":"); idx != -1 {
		lastSegment = lastSegment[:idx]
	}
	return lastSegment
}

// isVersionLikeTag is isVersionLikeTag (release-provider.ts:93): /^v?\d{1,4}$/
// or any "digit.digit" occurrence.
func isVersionLikeTag(tag string) bool {
	t := strings.TrimPrefix(tag, "v")
	if isDigits(t) && len(t) <= 4 {
		return true
	}
	for i := 0; i+2 < len(tag); i++ {
		if tag[i] >= '0' && tag[i] <= '9' && tag[i+1] == '.' && tag[i+2] >= '0' && tag[i+2] <= '9' {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// firstColonAfterLastSlash mirrors indexOf(":", lastIndexOf("/") + 1): the
// tag separator of an image reference (the registry port colon is skipped).
func firstColonAfterLastSlash(s string) int {
	from := strings.LastIndex(s, "/") + 1
	idx := strings.Index(s[from:], ":")
	if idx == -1 {
		return -1
	}
	return from + idx
}

// GetReleaseSourceFromDockerImage ports getReleaseSourceFromDockerImage
// (release-provider.ts:90-121): resolve the release provider and identifier
// of a container image reference; nil when the reference cannot be mapped
// (e.g. LinuxServer images pinned to a non-version tag).
func GetReleaseSourceFromDockerImage(image string) *ReleaseSource {
	withoutDigest := image
	if idx := strings.Index(image, "@"); idx != -1 {
		withoutDigest = image[:idx]
	}

	reference := withoutDigest
	version := ""
	if tagIndex := firstColonAfterLastSlash(withoutDigest); tagIndex != -1 {
		reference = withoutDigest[:tagIndex]
		version = withoutDigest[tagIndex+1:]
	}

	maybeSource := reference
	maybeIdentifier := ""
	if idx := strings.Index(reference, "/"); idx != -1 {
		maybeSource = reference[:idx]
		maybeIdentifier = reference[idx+1:]
	}
	_, hasSource := containerImageToProviderKind[maybeSource]
	source := "docker.io"
	if hasSource {
		source = maybeSource
	}
	identifier := reference
	if hasSource && maybeIdentifier != "" {
		identifier = maybeIdentifier
	}
	if identifier == "" {
		return nil
	}

	provider := containerImageToProviderKind[source]
	if provider == "" {
		provider = ReleaseProviderDockerHub
	}
	if provider == ReleaseProviderLinuxServerIO && version != "" && version != "latest" && !isVersionLikeTag(version) {
		return nil
	}
	identifier = normalizeReleaseProviderIdentifier(provider, identifier)
	if supportsReleaseProviderTracks(provider) && version != "" && version != "latest" && !isVersionLikeTag(version) {
		identifier = identifier + ":" + version
	}
	return &ReleaseSource{Provider: provider, Identifier: identifier, Version: version}
}

// supportsReleaseProviderTracks (release-provider.ts:88-90).
func supportsReleaseProviderTracks(provider ReleaseProviderKind) bool {
	return provider == ReleaseProviderDockerHub ||
		provider == ReleaseProviderGitHubContainerRegistry ||
		provider == ReleaseProviderQuay
}

// normalizeReleaseProviderIdentifier (release-provider.ts:86-87 +
// splitReleaseProviderReference :68-83): strip the registry prefix, then the
// digest, then any tag segment.
func normalizeReleaseProviderIdentifier(provider ReleaseProviderKind, identifier string) string {
	trimmed := strings.TrimSpace(identifier)
	prefix := releaseProviderRegistryPrefixes[provider]
	withoutRegistry := trimmed
	if prefix != "" && strings.HasPrefix(trimmed, prefix) {
		withoutRegistry = strings.TrimPrefix(trimmed, prefix)
	}
	if prefix == "" {
		return withoutRegistry
	}
	withoutDigest := withoutRegistry
	if idx := strings.Index(withoutRegistry, "@"); idx != -1 {
		withoutDigest = withoutRegistry[:idx]
	}
	if tagIndex := firstColonAfterLastSlash(withoutDigest); tagIndex != -1 {
		return withoutDigest[:tagIndex]
	}
	return withoutDigest
}

// ReleaseTokenKey derives the keyed-fingerprint HMAC key from
// SECRET_ENCRYPTION_KEY exactly like @homarr/common (packages/common/src/
// encryption.ts:9-10): HMAC-SHA256(secretKey, "homarr-keyed-fingerprint-v1"),
// with the 64-"0" hex fallback used when env validation is skipped (CI).
func ReleaseTokenKey() []byte {
	secret := os.Getenv("SECRET_ENCRYPTION_KEY")
	if secret == "" {
		secret = strings.Repeat("0", 64)
	}
	key, err := hex.DecodeString(secret)
	if err != nil {
		key = []byte(secret)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("homarr-keyed-fingerprint-v1"))
	return mac.Sum(nil)
}

// CreateKeyedFingerprint is createKeyedFingerprint (encryption.ts:36-38):
// hex HMAC-SHA256 under the derived key.
func CreateKeyedFingerprint(value string, key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// ReleaseSourceToken builds the releaseSourceToken of the getContainers
// response (docker-router.ts:89-98): keyed fingerprint of the JSON string
// ["releases-docker-suggestion-v1", <provider>, <identifier>]. Empty string
// when the image has no release source.
func ReleaseSourceToken(image string, key []byte) string {
	source := GetReleaseSourceFromDockerImage(image)
	if source == nil {
		return ""
	}
	payload := `["releases-docker-suggestion-v1","` + string(source.Provider) + `","` + source.Identifier + `"]`
	return CreateKeyedFingerprint(payload, key)
}
