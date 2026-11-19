package portal

import (
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var errIntelligenceAPIOrigins = errors.New("invalid_intelligence_api_origin_overrides")
var intelligenceOriginHostname = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// Overrides come only from deployment configuration. Plans never accept an
// arbitrary endpoint or persist this configuration alongside their credentials.
func intelligenceAPIOrigins(raw string) (map[int64]string, error) {
	origins := make(map[int64]string)
	if len(raw) > 8192 {
		return nil, errIntelligenceAPIOrigins
	}
	if strings.TrimSpace(raw) == "" {
		return origins, nil
	}
	d := json.NewDecoder(strings.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errIntelligenceAPIOrigins
	}
	for d.More() {
		key, err := d.Token()
		id, ok := key.(string)
		if err != nil || !ok || len(origins) >= 32 {
			return nil, errIntelligenceAPIOrigins
		}
		group, err := strconv.ParseInt(id, 10, 64)
		if err != nil || group <= 0 || strconv.FormatInt(group, 10) != id {
			return nil, errIntelligenceAPIOrigins
		}
		if _, exists := origins[group]; exists {
			return nil, errIntelligenceAPIOrigins
		}
		var value string
		if err := d.Decode(&value); err != nil {
			return nil, errIntelligenceAPIOrigins
		}
		parsed, err := url.Parse(strings.TrimSpace(value))
		// CanonicalOrigin rejects credentials, non-HTTPS, paths, private IPs
		// and nonempty queries/fragments. Also reject empty ?/# and encoded paths.
		if err != nil || parsed.ForceQuery || parsed.RawPath != "" || parsed.Opaque != "" || strings.Contains(value, "#") || strings.HasSuffix(parsed.Host, ":") {
			return nil, errIntelligenceAPIOrigins
		}
		origin, err := CanonicalOrigin(value)
		if err != nil {
			return nil, errIntelligenceAPIOrigins
		}
		canonical, _ := url.Parse(origin)
		host := canonical.Hostname()
		if _, err := netip.ParseAddr(host); err != nil && (len(host) > 253 || !intelligenceOriginHostname.MatchString(host)) {
			return nil, errIntelligenceAPIOrigins
		}
		origins[group] = origin + "/v1"
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return nil, errIntelligenceAPIOrigins
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errIntelligenceAPIOrigins
	}
	return origins, nil
}

func (s *IntelligenceService) apiBaseURL(groupID int64) string {
	if baseURL, ok := s.apiBaseURLs[groupID]; ok {
		return baseURL
	}
	return s.baseURL
}

type intelligencePlanSettings struct {
	IntelligencePlan
	APIBaseURL string `json:"api_base_url"`
}

func (s *IntelligenceService) settingsPlans(state intelligenceState) []intelligencePlanSettings {
	plans := orderedIntelligencePlans(state)
	out := make([]intelligencePlanSettings, 0, len(plans))
	for _, plan := range plans {
		out = append(out, intelligencePlanSettings{IntelligencePlan: plan, APIBaseURL: s.apiBaseURL(plan.GroupID)})
	}
	return out
}
