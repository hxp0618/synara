package gitpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"
)

var (
	ErrInvalidBranch       = errors.New("invalid Git branch")
	ErrInvalidRemote       = errors.New("invalid remote Git repository URL")
	ErrUnsafeRemoteAddress = errors.New("remote Git repository resolves to a non-public address")
)

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type Remote struct {
	URL      string
	Hostname string
	Port     string
	PinnedIP string
}

func NormalizeBranch(value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if value == "" || len(value) > 255 || strings.ContainsAny(value, " ~^:?*[\\") ||
		strings.Contains(value, "..") || strings.HasPrefix(value, ".") ||
		strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || value == "@" ||
		strings.HasSuffix(value, ".") || strings.HasSuffix(value, ".lock") ||
		strings.Contains(value, "@{") || strings.Contains(value, "//") {
		return "", ErrInvalidBranch
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return "", ErrInvalidBranch
		}
	}
	return value, nil
}

func ValidateRemoteHTTPS(ctx context.Context, resolver Resolver, raw string) (string, error) {
	remote, err := ResolveRemoteHTTPS(ctx, resolver, raw)
	if err != nil {
		return "", err
	}
	return remote.URL, nil
}

func ResolveRemoteHTTPS(ctx context.Context, resolver Resolver, raw string) (Remote, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\t ") {
		return Remote{}, ErrInvalidRemote
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Remote{}, ErrInvalidRemote
	}
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if hostname == "" || hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return Remote{}, ErrUnsafeRemoteAddress
	}
	decodedPath, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || decodedPath == "" || decodedPath == "/" || path.Clean(decodedPath) != decodedPath {
		return Remote{}, ErrInvalidRemote
	}
	for _, segment := range strings.Split(decodedPath, "/") {
		if segment == "." || segment == ".." {
			return Remote{}, ErrInvalidRemote
		}
	}
	explicitPort := parsed.Port()
	if explicitPort != "" {
		portNumber, portErr := strconv.Atoi(explicitPort)
		if portErr != nil || portNumber < 1 || portNumber > 65535 {
			return Remote{}, ErrInvalidRemote
		}
	}
	var pinnedIP string
	if ip := net.ParseIP(hostname); ip != nil {
		if !isPublicIP(ip) {
			return Remote{}, ErrUnsafeRemoteAddress
		}
		pinnedIP = ip.String()
	} else {
		if resolver == nil {
			return Remote{}, ErrUnsafeRemoteAddress
		}
		addresses, resolveErr := resolver.LookupIPAddr(ctx, hostname)
		if resolveErr != nil || len(addresses) == 0 {
			return Remote{}, ErrUnsafeRemoteAddress
		}
		for _, address := range addresses {
			if !isPublicIP(address.IP) {
				return Remote{}, ErrUnsafeRemoteAddress
			}
		}
		pinnedIP = preferredIP(addresses)
	}
	parsed.Scheme = "https"
	parsed.Host = hostname
	if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	}
	if explicitPort != "" {
		parsed.Host = net.JoinHostPort(hostname, explicitPort)
	}
	parsed.Path = decodedPath
	parsed.RawPath = ""
	port := explicitPort
	if port == "" {
		port = "443"
	}
	return Remote{URL: parsed.String(), Hostname: hostname, Port: port, PinnedIP: pinnedIP}, nil
}

func preferredIP(addresses []net.IPAddr) string {
	for _, address := range addresses {
		if address.IP.To4() != nil {
			return address.IP.String()
		}
	}
	return addresses[0].IP.String()
}

func Fingerprint(remoteURL string) string {
	digest := sha256.Sum256([]byte(remoteURL))
	return hex.EncodeToString(digest[:])
}

func isPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}
