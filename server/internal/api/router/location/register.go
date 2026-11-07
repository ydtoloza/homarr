// Package location is the Go port of the location tRPC router
// (packages/api/src/router/location.ts): a proxy of the Open-Meteo geocoding
// API (https://geocoding-api.open-meteo.com/v1/search) that the weather
// widget uses to resolve a city search.
//
// The upstream procedure is public, takes {query} and pipes the response
// through an OUTPUT schema (locationSearchCityOutput, location.ts:24-41):
//
//   - the happy shape is {results: [city...]}, where every city carries
//     exactly {id, name, latitude, longitude} plus the optional {country,
//     country_code, population} — the zod object STRIPS every other key the
//     API returns, and the optionals are dropped when absent (superjson);
//   - a response that is only {generationtime_ms: <number>} (the API answers
//     that when nothing matched) falls back to {results: []} via the refine +
//     transform; any other shape fails the output validation, which tRPC
//     surfaces as INTERNAL_SERVER_ERROR.
//
// There is deliberately no res.ok check upstream: an API error body
// ({error: true, reason}) simply fails the output schema. The port mirrors
// that. env.NO_EXTERNAL_CONNECTION (upstream test env returning empty
// results) is not ported, like the other external proxies.
package location

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
	"github.com/ydtoloza/homarr/server/internal/db"
)

// DefaultGeocodingURL is the endpoint the procedure requests
// (location.ts:47).
const DefaultGeocodingURL = "https://geocoding-api.open-meteo.com/v1/search"

// fetchTimeout is the withTimeoutAsync default (timeout.ts:6), the same
// bound the other external proxies of the port use.
const fetchTimeout = 10 * time.Second

// maxBodyBytes bounds the response read (the real payload is a few KB).
const maxBodyBytes = 4 << 20

// Options configures the router. The zero value talks to the real endpoint.
type Options struct {
	// GeocodingURL overrides the Open-Meteo endpoint (test seam).
	GeocodingURL string
	// Client overrides the HTTP client (tests).
	Client *http.Client
}

// Register registers the location router. Call once at startup. The database
// is unused (the search is a live proxy).
func Register(r *trpc.Router, database *db.DB, opts Options) {
	_ = database
	if opts.GeocodingURL == "" {
		opts.GeocodingURL = DefaultGeocodingURL
	}
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	s := &service{opts: opts}

	// location.searchCity (location.ts:43-53): publicProcedure.
	r.Register("location.searchCity", s.searchCity)
}

type service struct {
	opts Options
}

// city is citySchema (location.ts:9-16) with the optional members omitted
// when absent (zod optional + superjson drop the undefined keys). Unknown
// keys of the API response are stripped, exactly like the zod object.
type city struct {
	ID          float64  `json:"id"`
	Name        string   `json:"name"`
	Country     *string  `json:"country,omitempty"`
	CountryCode *string  `json:"country_code,omitempty"`
	Latitude    float64  `json:"latitude"`
	Longitude   float64  `json:"longitude"`
	Population  *float64 `json:"population,omitempty"`
}

// searchCity mirrors the procedure body (location.ts:45-53): fetch with the
// name parameter and validate the output shape.
func (s *service) searchCity(ctx context.Context, input json.RawMessage) (any, *trpc.Error) {
	d, terr := admin.NewInputDecoder(input)
	if terr != nil {
		return nil, terr
	}
	query, terr := d.Str("query")
	if terr != nil {
		return nil, terr
	}

	body, terr := s.fetch(ctx, query)
	if terr != nil {
		return nil, terr
	}
	return validateOutput(body)
}

// fetch GETs the geocoding search with the name parameter.
func (s *service) fetch(ctx context.Context, query string) ([]byte, *trpc.Error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	u, err := url.Parse(s.opts.GeocodingURL)
	if err != nil {
		return nil, trpc.Internal(fmt.Errorf("location: invalid geocoding url: %w", err))
	}
	q := u.Query()
	q.Set("name", query)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, trpc.Internal(fmt.Errorf("location: build request: %w", err))
	}
	res, err := s.opts.Client.Do(req)
	if err != nil {
		return nil, trpc.Internal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return nil, trpc.Internal(err)
	}
	return raw, nil
}

// validateOutput is locationSearchCityOutput.parseAsync (location.ts:24-41):
// the results shape wins; a bare {generationtime_ms} object falls back to
// {results: []}; everything else is an output validation failure
// (INTERNAL_SERVER_ERROR).
func validateOutput(body []byte) (any, *trpc.Error) {
	var probe struct {
		Results        json.RawMessage `json:"results"`
		Generationtime *float64        `json:"generationtime_ms"`
		ExtraKeys      int
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, internalOutputError(err)
	}

	if probe.Results != nil {
		var rawCities []json.RawMessage
		if err := json.Unmarshal(probe.Results, &rawCities); err != nil {
			return nil, internalOutputError(err)
		}
		cities := make([]city, 0, len(rawCities))
		for i, raw := range rawCities {
			c, err := decodeCity(raw)
			if err != nil {
				return nil, internalOutputError(fmt.Errorf("city %d: %w", i, err))
			}
			cities = append(cities, c)
		}
		return map[string]any{"results": cities}, nil
	}

	// The generationtime fallback requires the object to carry ONLY that key
	// (the refine of location.ts:32-35).
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		return nil, internalOutputError(err)
	}
	if len(keys) == 1 && probe.Generationtime != nil {
		return map[string]any{"results": []city{}}, nil
	}
	return nil, internalOutputError(fmt.Errorf("invalid response"))
}

// decodeCity parses one city with the zod required/optional rules: id, name,
// latitude and longitude are required numbers/strings, the optionals pass
// through when present, unknown keys are stripped.
func decodeCity(raw json.RawMessage) (city, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return city{}, err
	}
	var out city
	var missing []string
	if v, ok := m["id"]; ok {
		if err := json.Unmarshal(v, &out.ID); err != nil {
			return city{}, fmt.Errorf("id: %w", err)
		}
	} else {
		missing = append(missing, "id")
	}
	if v, ok := m["name"]; ok {
		if err := json.Unmarshal(v, &out.Name); err != nil {
			return city{}, fmt.Errorf("name: %w", err)
		}
	} else {
		missing = append(missing, "name")
	}
	if v, ok := m["latitude"]; ok {
		if err := json.Unmarshal(v, &out.Latitude); err != nil {
			return city{}, fmt.Errorf("latitude: %w", err)
		}
	} else {
		missing = append(missing, "latitude")
	}
	if v, ok := m["longitude"]; ok {
		if err := json.Unmarshal(v, &out.Longitude); err != nil {
			return city{}, fmt.Errorf("longitude: %w", err)
		}
	} else {
		missing = append(missing, "longitude")
	}
	if len(missing) > 0 {
		return city{}, fmt.Errorf("missing required keys: %s", strings.Join(missing, ", "))
	}
	// Optionals: present-and-typed only (a null optional is treated as
	// absent, an edge the API never produces).
	if v, ok := m["country"]; ok && string(v) != "null" {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out.Country = &s
		} else {
			return city{}, fmt.Errorf("country: %w", err)
		}
	}
	if v, ok := m["country_code"]; ok && string(v) != "null" {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out.CountryCode = &s
		} else {
			return city{}, fmt.Errorf("country_code: %w", err)
		}
	}
	if v, ok := m["population"]; ok && string(v) != "null" {
		var n float64
		if err := json.Unmarshal(v, &n); err == nil {
			out.Population = &n
		} else {
			return city{}, fmt.Errorf("population: %w", err)
		}
	}
	return out, nil
}

// internalOutputError maps an output validation failure to the tRPC error —
// upstream surfaces zod output failures as INTERNAL_SERVER_ERROR with the
// zod message.
func internalOutputError(err error) *trpc.Error {
	return trpc.Internal(fmt.Errorf("location: invalid output: %w", err))
}
