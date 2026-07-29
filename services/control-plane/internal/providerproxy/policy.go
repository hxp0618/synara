package providerproxy

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
)

type Mode uint8

const (
	HTTPOrHTTPS Mode = iota
	HTTPHTTPSOrSOCKS5
)

var (
	ErrInvalidAuthority  = errors.New("Provider proxy must be a credential-free authority")
	ErrUnsupportedScheme = errors.New("Provider proxy scheme is unsupported")
	ErrInvalidHost       = errors.New("Provider proxy host is invalid")
	ErrInvalidPort       = errors.New("Provider proxy port is invalid")
	ErrSOCKS5Port        = errors.New("SOCKS5 Provider proxy requires an explicit port")
	ErrInvalidNoProxy    = errors.New("Provider no-proxy contains an invalid entry")
)

func Normalize(raw string, mode Mode) (string, int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil ||
		strings.ContainsAny(raw, "?#") || (parsed.Path != "" && parsed.Path != "/") {
		return "", 0, ErrInvalidAuthority
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if !allowedScheme(parsed.Scheme, mode) {
		return "", 0, ErrUnsupportedScheme
	}
	if _, err := gitpolicy.NormalizeHostname(parsed.Hostname()); err != nil {
		return "", 0, ErrInvalidHost
	}
	port := 0
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", 0, ErrInvalidPort
		}
	} else if parsed.Scheme == "http" {
		port = 80
	} else if parsed.Scheme == "https" {
		port = 443
	} else {
		return "", 0, ErrSOCKS5Port
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return parsed.String(), port, nil
}

func NormalizeNoProxy(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	entries := strings.Split(raw, ",")
	if len(entries) > 64 {
		return "", ErrInvalidNoProxy
	}
	for index, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" || entry == "*" || len(entry) > 253 || strings.ContainsAny(entry, "\r\n\t\x00") {
			return "", ErrInvalidNoProxy
		}
		entries[index] = entry
	}
	return strings.Join(entries, ","), nil
}

func allowedScheme(scheme string, mode Mode) bool {
	switch scheme {
	case "http", "https":
		return true
	case "socks5":
		return mode == HTTPHTTPSOrSOCKS5
	default:
		return false
	}
}
