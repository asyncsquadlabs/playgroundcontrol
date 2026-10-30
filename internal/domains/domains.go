// Package domains normalizes browser hosts and identifies known game domains.
package domains

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// SeedDomains is the deterministic game-domain list embedded in the build. A
// matched domain is blocked without consulting AI.
var SeedDomains = []string{
	"crazygames.com",
	"bloxd.io",
	"poki.com",
	"friv.com",
	"miniclip.com",
	"roblox.com",
	"y8.com",
	"kongregate.com",
	"coolmathgames.com",
	"silvergames.com",
	"addictinggames.com",
	"armorgames.com",
	"newgrounds.com",
	"agame.com",
	"gamepix.com",
}

// ErrInvalidDomain reports that a value is not a usable public browser host.
var ErrInvalidDomain = errors.New("invalid browser domain")

// Normalize accepts a bare host or an http(s) URL and returns the lowercase
// registrable domain. Full paths, ports, query strings, fragments, and
// credentials are discarded. IP literals, local names, and single-label hosts
// are rejected.
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalidDomain
	}
	if len(raw) > 2048 {
		return "", ErrInvalidDomain
	}

	value := raw
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", ErrInvalidDomain
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrInvalidDomain
	}
	if parsed.User != nil {
		return "", ErrInvalidDomain
	}
	host := parsed.Hostname()
	if host == "" {
		return "", ErrInvalidDomain
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")

	if ip := net.ParseIP(host); ip != nil {
		return "", fmt.Errorf("%w: IP addresses are not monitored", ErrInvalidDomain)
	}
	switch {
	case host == "localhost",
		strings.HasSuffix(host, ".localhost"),
		strings.HasSuffix(host, ".local"),
		strings.HasSuffix(host, ".localdomain"),
		strings.HasSuffix(host, ".lan"),
		strings.HasSuffix(host, ".internal"),
		strings.HasSuffix(host, ".home.arpa"):
		return "", fmt.Errorf("%w: local host names are not monitored", ErrInvalidDomain)
	}

	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", ErrInvalidDomain
	}
	ascii = strings.ToLower(strings.TrimSuffix(ascii, "."))
	if err := validateHost(ascii); err != nil {
		return "", err
	}

	suffix, _ := publicsuffix.PublicSuffix(ascii)
	if strings.EqualFold(ascii, suffix) {
		return "", fmt.Errorf("%w: a public suffix is not a registrable domain", ErrInvalidDomain)
	}
	registered, err := publicsuffix.EffectiveTLDPlusOne(ascii)
	if err != nil {
		// Unknown private or test suffixes still produce a usable multi-label
		// host, so accept the normalized host when it has at least two labels.
		if strings.Count(ascii, ".") >= 1 {
			registered = ascii
		} else {
			return "", ErrInvalidDomain
		}
	}
	registered = strings.TrimSuffix(strings.ToLower(registered), ".")
	if err := validateHost(registered); err != nil {
		return "", err
	}
	if !strings.Contains(registered, ".") {
		return "", ErrInvalidDomain
	}
	return registered, nil
}

func validateHost(host string) error {
	if host == "" || len(host) > 253 {
		return ErrInvalidDomain
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: a multi-label host is required", ErrInvalidDomain)
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return ErrInvalidDomain
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return ErrInvalidDomain
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return ErrInvalidDomain
		}
	}
	return nil
}

// IsSeed reports whether a normalized domain is in the deterministic list.
func IsSeed(domain string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	for _, seed := range SeedDomains {
		if domain == seed {
			return true
		}
	}
	return false
}

// Seeds returns a copy of the embedded seed list.
func Seeds() []string {
	out := make([]string, len(SeedDomains))
	copy(out, SeedDomains)
	return out
}
