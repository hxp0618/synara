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

type AddressPolicy struct {
	privateNetworks []*net.IPNet
}

// ParsePrivateNetworkCIDRs creates an explicit exception to the default SSRF
// boundary for operator-owned enterprise networks. Only RFC1918, carrier-grade
// NAT, or IPv6 ULA ranges are accepted; loopback, link-local, metadata, and
// public ranges can never be enabled through this policy.
func ParsePrivateNetworkCIDRs(values []string) (AddressPolicy, error) {
	if len(values) > 64 {
		return AddressPolicy{}, ErrUnsafeRemoteAddress
	}
	allowedParents := mustCIDRs(
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7",
	)
	result := AddressPolicy{privateNetworks: make([]*net.IPNet, 0, len(values))}
	seen := map[string]struct{}{}
	for _, value := range values {
		ip, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil || !ip.Equal(network.IP) || !networkContainedByAny(network, allowedParents) {
			return AddressPolicy{}, ErrUnsafeRemoteAddress
		}
		canonical := network.String()
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		result.privateNetworks = append(result.privateNetworks, network)
	}
	return result, nil
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
	return ResolveRemoteWithAddressPolicy(ctx, resolver, raw, AddressPolicy{})
}

func ResolveRemoteWithAddressPolicy(
	ctx context.Context,
	resolver Resolver,
	raw string,
	policy AddressPolicy,
) (Remote, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Opaque != "" {
		return Remote{}, ErrInvalidRemote
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return resolveRemoteHTTPS(ctx, resolver, raw, policy)
	case "ssh":
		return resolveRemoteSSH(ctx, resolver, raw, policy)
	default:
		return Remote{}, ErrInvalidRemote
	}
}

func ResolveRemoteHTTPS(ctx context.Context, resolver Resolver, raw string) (Remote, error) {
	return resolveRemoteHTTPS(ctx, resolver, raw, AddressPolicy{})
}

func resolveRemoteHTTPS(ctx context.Context, resolver Resolver, raw string, policy AddressPolicy) (Remote, error) {
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
	pinnedIP, err := resolveAllowedAddress(ctx, resolver, hostname, policy)
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
	return resolveRemoteSSH(ctx, resolver, raw, AddressPolicy{})
}

func resolveRemoteSSH(ctx context.Context, resolver Resolver, raw string, policy AddressPolicy) (Remote, error) {
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
	pinnedIP, err := resolveAllowedAddress(ctx, resolver, hostname, policy)
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
	return resolveAllowedAddress(ctx, resolver, hostname, AddressPolicy{})
}

func resolveAllowedAddress(ctx context.Context, resolver Resolver, hostname string, policy AddressPolicy) (string, error) {
	if ip := net.ParseIP(hostname); ip != nil {
		if !isPublicIP(ip) && !policy.allowsPrivate(ip) {
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
		if !isPublicIP(address.IP) && !policy.allowsPrivate(address.IP) {
			return "", ErrUnsafeRemoteAddress
		}
	}
	return preferredIP(addresses), nil
}

func (p AddressPolicy) allowsPrivate(ip net.IP) bool {
	if forbiddenRemoteIP(ip) {
		return false
	}
	for _, network := range p.privateNetworks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func forbiddenRemoteIP(ip net.IP) bool {
	return ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

func mustCIDRs(values ...string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, _ := net.ParseCIDR(value)
		result = append(result, network)
	}
	return result
}

func networkContainedByAny(candidate *net.IPNet, parents []*net.IPNet) bool {
	last := append(net.IP(nil), candidate.IP...)
	for index := range last {
		last[index] |= ^candidate.Mask[index]
	}
	for _, parent := range parents {
		if parent.Contains(candidate.IP) && parent.Contains(last) {
			return true
		}
	}
	return false
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
