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
	Scheme   string
	Hostname string
	Port     string
	PinnedIP string
	Username string
}

func NormalizeHostname(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if value == "" || len(value) > 253 || strings.ContainsAny(value, "/\\@?#[]") {
		return "", ErrInvalidRemote
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return "", ErrInvalidRemote
		}
	}
	if ip := net.ParseIP(value); ip != nil {
		return strings.ToLower(ip.String()), nil
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidRemote
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return "", ErrInvalidRemote
			}
		}
	}
	return value, nil
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

func ValidateRemote(ctx context.Context, resolver Resolver, raw string) (string, error) {
	remote, err := ResolveRemote(ctx, resolver, raw)
	if err != nil {
		return "", err
	}
	return remote.URL, nil
}

func ValidateRemoteSSH(ctx context.Context, resolver Resolver, raw string) (string, error) {
	remote, err := ResolveRemoteSSH(ctx, resolver, raw)
	if err != nil {
		return "", err
	}
	return remote.URL, nil
}

func ResolveRemote(ctx context.Context, resolver Resolver, raw string) (Remote, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Opaque != "" {
		return Remote{}, ErrInvalidRemote
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return ResolveRemoteHTTPS(ctx, resolver, raw)
	case "ssh":
		return ResolveRemoteSSH(ctx, resolver, raw)
	default:
		return Remote{}, ErrInvalidRemote
	}
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
	hostname, decodedPath, err := normalizeRemoteEndpoint(parsed)
	if err != nil {
		return Remote{}, err
	}
	explicitPort := parsed.Port()
	if explicitPort != "" {
		portNumber, portErr := strconv.Atoi(explicitPort)
		if portErr != nil || portNumber < 1 || portNumber > 65535 {
			return Remote{}, ErrInvalidRemote
		}
	}
	pinnedIP, err := resolvePublicAddress(ctx, resolver, hostname)
	if err != nil {
		return Remote{}, err
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
	return Remote{
		URL: parsed.String(), Scheme: "https", Hostname: hostname, Port: port, PinnedIP: pinnedIP,
	}, nil
}

func ResolveRemoteSSH(ctx context.Context, resolver Resolver, raw string) (Remote, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\t ") {
		return Remote{}, ErrInvalidRemote
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "ssh") ||
		parsed.Host == "" || parsed.User == nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Remote{}, ErrInvalidRemote
	}
	if _, present := parsed.User.Password(); present {
		return Remote{}, ErrInvalidRemote
	}
	username := parsed.User.Username()
	if !validSSHUsername(username) {
		return Remote{}, ErrInvalidRemote
	}
	hostname, decodedPath, err := normalizeRemoteEndpoint(parsed)
	if err != nil {
		return Remote{}, err
	}
	explicitPort := parsed.Port()
	if explicitPort != "" {
		portNumber, portErr := strconv.Atoi(explicitPort)
		if portErr != nil || portNumber < 1 || portNumber > 65535 {
			return Remote{}, ErrInvalidRemote
		}
	}
	pinnedIP, err := resolvePublicAddress(ctx, resolver, hostname)
	if err != nil {
		return Remote{}, err
	}
	parsed.Scheme = "ssh"
	parsed.User = url.User(username)
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
		port = "22"
	}
	return Remote{
		URL: parsed.String(), Scheme: "ssh", Hostname: hostname, Port: port,
		PinnedIP: pinnedIP, Username: username,
	}, nil
}

func normalizeRemoteEndpoint(parsed *url.URL) (string, string, error) {
	hostname, err := NormalizeHostname(parsed.Hostname())
	if err != nil {
		return "", "", ErrInvalidRemote
	}
	if hostname == "" || hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return "", "", ErrUnsafeRemoteAddress
	}
	decodedPath, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || decodedPath == "" || decodedPath == "/" || path.Clean(decodedPath) != decodedPath {
		return "", "", ErrInvalidRemote
	}
	for _, segment := range strings.Split(decodedPath, "/") {
		if segment == "." || segment == ".." {
			return "", "", ErrInvalidRemote
		}
	}
	return hostname, decodedPath, nil
}

func resolvePublicAddress(ctx context.Context, resolver Resolver, hostname string) (string, error) {
	if ip := net.ParseIP(hostname); ip != nil {
		if !isPublicIP(ip) {
			return "", ErrUnsafeRemoteAddress
		}
		return ip.String(), nil
	}
	if resolver == nil {
		return "", ErrUnsafeRemoteAddress
	}
	addresses, err := resolver.LookupIPAddr(ctx, hostname)
	if err != nil || len(addresses) == 0 {
		return "", ErrUnsafeRemoteAddress
	}
	for _, address := range addresses {
		if !isPublicIP(address.IP) {
			return "", ErrUnsafeRemoteAddress
		}
	}
	return preferredIP(addresses), nil
}

func validSSHUsername(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) || strings.ContainsRune("/@:\\", character) {
			return false
		}
	}
	return true
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
