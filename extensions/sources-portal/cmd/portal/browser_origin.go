package main

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"

	"sources.local/portal/internal/portal"
)

// canonicalBrowserOrigin accepts only authorities whose browser interpretation
// needs at most ASCII case folding and removal of the default HTTPS port.
// Reject ambiguous spellings instead of approximating the browser's IDNA and
// legacy IPv4 parsers. Keep this policy local to the startup login boundary.
func canonicalBrowserOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !canonicalBrowserAuthority(u) {
		return "", errors.New("canonical ASCII URL host and unpadded port required")
	}
	return portal.CanonicalOrigin(raw)
}

func canonicalBrowserAuthority(u *url.URL) bool {
	host, port := u.Hostname(), u.Port()
	if host == "" || len(port) > 1 && port[0] == '0' {
		return false
	}
	authority := host
	if addr, err := netip.ParseAddr(host); err == nil {
		// Do not let URL parsing and IP parsing disagree on zones, mapped
		// addresses, expanded IPv6 or any other noncanonical IP spelling.
		if addr.Zone() != "" || addr.Is4In6() || !strings.EqualFold(host, addr.String()) {
			return false
		}
		if addr.Is6() {
			authority = "[" + host + "]"
		}
	} else {
		if len(host) > 253 {
			return false
		}
		labels := strings.Split(host, ".")
		for _, label := range labels {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for i := range label {
				c := label[i]
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					return false
				}
			}
		}
		// A browser routes numeric-looking final labels through its IPv4
		// parser, including integer, octal and hexadecimal address aliases.
		last := strings.ToLower(labels[len(labels)-1])
		if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "" {
			return false
		}
	}
	if port != "" {
		authority += ":" + port
	}
	// Reject empty ports and brackets around DNS names as well as any
	// authority syntax not accounted for above.
	return strings.EqualFold(u.Host, authority)
}
