package main

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"sources.local/portal/internal/portal"
)

var errBrowserOrigin = errors.New("canonical HTTPS origin required")

// The main-site origin also serves outbound Portal requests, so it keeps the
// existing public address policy after browser syntax validation.
func canonicalMainSiteOrigin(raw string) (string, error) {
	origin, err := canonicalBrowserOrigin(raw)
	if err != nil {
		return "", err
	}
	return portal.CanonicalOrigin(origin)
}

// canonicalBrowserOrigin compares browser page origins without applying the
// outbound public address policy. Ambiguous DNS and legacy IPv4 forms are
// rejected, while equivalent IPv6 spellings are normalized.
func canonicalBrowserOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errBrowserOrigin
	}
	authority, ok := canonicalBrowserAuthority(u)
	if !ok {
		return "", errBrowserOrigin
	}
	return "https://" + authority, nil
}

func canonicalBrowserAuthority(u *url.URL) (string, bool) {
	host, port := u.Hostname(), u.Port()
	authority, normalized := host, strings.ToLower(host)
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" || addr.Is4In6() {
			return "", false
		}
		if addr.Is6() {
			authority = "[" + host + "]"
			normalized = "[" + addr.String() + "]"
		}
	} else {
		if !validDNSHostname(host) {
			return "", false
		}
		// A browser routes numeric-looking final labels through its IPv4
		// parser, including integer, octal and hexadecimal address aliases.
		last := normalized[strings.LastIndexByte(normalized, '.')+1:]
		if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "" {
			return "", false
		}
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || port != strconv.Itoa(n) {
			return "", false
		}
		authority += ":" + port
		if port != "443" {
			normalized += ":" + port
		}
	}
	// Reject empty ports and brackets around DNS names.
	return normalized, u.Host == authority
}

func validDNSHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
