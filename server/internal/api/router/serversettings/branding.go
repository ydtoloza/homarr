package serversettings

import (
	"regexp"
	"strings"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
)

// Branding settings validation, ported from
// packages/server-settings/src/index.ts (brandingServerSettingsSchema,
// parseBrandingSettings, brandingServerSettingsUpdateSchema).

var (
	hexColorPattern      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	cornerStyleValues    = []string{"xs", "sm", "md", "lg", "xl"}
	brandingStringFields = map[string]int{ // field -> max length (trim first)
		"appName":   80,
		"greeting":  160,
		"customCss": 16384,
	}
)

// parseBrandingSettings is parseBrandingSettings
// (packages/server-settings/src/index.ts:104-128): the stored record merges
// over the full defaults (with the legacy authBranding keys folded in) and
// the result validates against brandingServerSettingsSchema; ANY failure
// falls back to the untouched defaults.
func parseBrandingSettings(value map[string]any) map[string]any {
	if value == nil {
		return defaultsFor("branding")
	}
	// authBranding resolution (index.ts:108-118): defaults <- legacy keys
	// <- structured authBranding patch.
	authBranding := map[string]any{
		"showAppName":  true,
		"showLogo":     true,
		"showGreeting": true,
	}
	if legacy, ok := value["showCustomAppNameOnLogin"].(bool); ok {
		authBranding["showAppName"] = legacy
	}
	if legacy, ok := value["showCustomLogoOnLogin"].(bool); ok {
		authBranding["showLogo"] = legacy
	}
	if legacy, ok := value["showCustomGreetingOnLogin"].(bool); ok {
		authBranding["showGreeting"] = legacy
	}
	if nested, ok := value["authBranding"].(map[string]any); ok {
		for k, v := range nested {
			authBranding[k] = v
		}
	}

	merged := defaultsFor("branding")
	for k, v := range value {
		merged[k] = v
	}
	merged["authBranding"] = authBranding
	if validateBrandingFull(merged) != nil {
		return defaultsFor("branding")
	}
	return merged
}

// validateBrandingUpdate is brandingServerSettingsUpdateSchema
// (serverSettings.ts:33-35): the partial branding fields plus the optional
// structured authBranding. Field types must be right; unknown keys are
// dropped (non-strict zod object) — except they may carry the legacy keys,
// which the caller reads separately.
func validateBrandingUpdate(d *admin.InputDecoder) (map[string]any, *trpc.Error) {
	patch := map[string]any{}
	for field, maxLen := range brandingStringFields {
		if v, ok, terr := d.OptStr(field); terr != nil {
			return nil, terr
		} else if ok {
			trimmed := strings.TrimSpace(v)
			if len(trimmed) > maxLen {
				return nil, admin.BadRequestf("branding settings %s is too long", field)
			}
			patch[field] = trimmed
		}
	}
	for _, field := range []string{"logoImageUrl", "faviconImageUrl", "signInBackgroundImageUrl"} {
		if v, ok, terr := d.StringPtr(field); terr != nil {
			return nil, terr
		} else if ok && v != nil {
			trimmed := strings.TrimSpace(*v)
			if len(trimmed) > 2048 {
				return nil, admin.BadRequestf("branding settings %s is too long", field)
			}
			if trimmed != "" && !isImageURLValue(trimmed) {
				return nil, admin.BadRequestf("branding settings %s must be an HTTP(S) URL or an app-relative path", field)
			}
			if trimmed == "" {
				patch[field] = nil
			} else {
				patch[field] = trimmed
			}
		}
	}
	for _, field := range []string{"primaryColor", "secondaryColor"} {
		if v, ok, terr := d.OptStr(field); terr != nil {
			return nil, terr
		} else if ok {
			if !hexColorPattern.MatchString(v) {
				return nil, admin.BadRequestf("branding settings %s must be a 6-digit hex color", field)
			}
			patch[field] = v
		}
	}
	for _, field := range []string{"lockPrimaryColor", "showSignInMarquee"} {
		if v, ok, terr := d.OptBool(field); terr != nil {
			return nil, terr
		} else if ok {
			patch[field] = v
		}
	}
	if v, ok, terr := d.OptStr("defaultRadius"); terr != nil {
		return nil, terr
	} else if ok {
		if !admin.ContainsString(cornerStyleValues, v) {
			return nil, admin.BadRequestf("branding settings defaultRadius has an invalid value")
		}
		patch["defaultRadius"] = v
	}
	if v, ok, terr := d.OptNumberRange("signInBackgroundOverlay", 0, 0.9); terr != nil {
		return nil, terr
	} else if ok {
		patch["signInBackgroundOverlay"] = v
	}
	if nested, terr := d.Object("authBranding"); terr != nil {
		return nil, terr
	} else if len(nested.All()) > 0 {
		authPatch := map[string]any{}
		for _, field := range []string{"showAppName", "showLogo", "showGreeting"} {
			if b, ok, terr := nested.OptBool(field); terr != nil {
				return nil, terr
			} else if ok {
				authPatch[field] = b
			}
		}
		patch["authBranding"] = authPatch
	}
	return patch, nil
}

// isImageURLValue is the brandingImageUrlSchema refine (index.ts:28-41):
// an app-relative path ("/..." but not "//...") or an HTTP(S) URL.
func isImageURLValue(value string) bool {
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return true
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return true
	}
	return false
}

// validateBrandingFull is brandingServerSettingsSchema applied to a merged
// record (index.ts:43-57); parseBrandingSettings falls back to the defaults
// when this fails.
func validateBrandingFull(settings map[string]any) *trpc.Error {
	for field, maxLen := range brandingStringFields {
		v, ok := settings[field].(string)
		if !ok {
			return admin.BadRequestf("branding settings %s must be a string", field)
		}
		if len(strings.TrimSpace(v)) > maxLen || field == "appName" && len(strings.TrimSpace(v)) < 1 {
			return admin.BadRequestf("branding settings %s has an invalid length", field)
		}
	}
	for _, field := range []string{"logoImageUrl", "faviconImageUrl", "signInBackgroundImageUrl"} {
		switch v := settings[field].(type) {
		case nil:
			// ok
		case string:
			if v != "" && (len(strings.TrimSpace(v)) > 2048 || !isImageURLValue(v)) {
				return admin.BadRequestf("branding settings %s must be an HTTP(S) URL or an app-relative path", field)
			}
		default:
			return admin.BadRequestf("branding settings %s must be a string or null", field)
		}
	}
	for _, field := range []string{"primaryColor", "secondaryColor"} {
		v, ok := settings[field].(string)
		if !ok || !hexColorPattern.MatchString(v) {
			return admin.BadRequestf("branding settings %s must be a 6-digit hex color", field)
		}
	}
	for _, field := range []string{"lockPrimaryColor", "showSignInMarquee"} {
		if _, ok := settings[field].(bool); !ok {
			return admin.BadRequestf("branding settings %s must be a boolean", field)
		}
	}
	overlay, ok := settings["signInBackgroundOverlay"].(float64)
	if !ok || overlay < 0 || overlay > 0.9 {
		return admin.BadRequestf("branding settings signInBackgroundOverlay must be between 0 and 0.9")
	}
	radius, ok := settings["defaultRadius"].(string)
	if !ok || !admin.ContainsString(cornerStyleValues, radius) {
		return admin.BadRequestf("branding settings defaultRadius has an invalid value")
	}
	authBranding, ok := settings["authBranding"].(map[string]any)
	if !ok {
		return admin.BadRequestf("branding settings authBranding must be an object")
	}
	for _, field := range []string{"showAppName", "showLogo", "showGreeting"} {
		if _, isBool := authBranding[field].(bool); !isBool {
			return admin.BadRequestf("branding settings authBranding.%s must be a boolean", field)
		}
	}
	return nil
}
