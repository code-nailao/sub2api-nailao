package main

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"sources.local/portal/internal/portal"
)

var errBrowserOrigin = errors.New("canonical HTTP or HTTPS origin required")

// The main-site origin also serves outbound Portal requests, so it keeps the
// existing public address policy after browser syntax validation.
func canonicalMainSiteOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errBrowserOrigin
	}
	origin, err := canonicalBrowserOrigin(u)
	if err != nil {
		return "", err
	}
	return portal.CanonicalOrigin(origin)
}

// canonicalBrowserOrigin serializes (scheme, host, port), omitting the
// default port for the scheme. It does not resolve the host or require it
// to be public. Page and outbound URL policies belong to its callers.
func canonicalBrowserOrigin(u *url.URL) (string, error) {
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", errBrowserOrigin
	}
	authority, ok := canonicalBrowserAuthority(u)
	if !ok {
		return "", errBrowserOrigin
	}
	return u.Scheme + "://" + authority, nil
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
		if !(u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80") {
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
