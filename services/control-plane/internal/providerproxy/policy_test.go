package providerproxy

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeAcceptsCredentialFreeProxyAuthorities(t *testing.T) {
	for _, test := range []struct {
		raw  string
		mode Mode
		port int
	}{
		{raw: "http://proxy.example.test", mode: HTTPOrHTTPS, port: 80},
		{raw: "https://proxy.example.test:8443/", mode: HTTPOrHTTPS, port: 8443},
		{raw: "socks5://127.0.0.1:1080", mode: HTTPHTTPSOrSOCKS5, port: 1080},
		{raw: "http://[2001:db8::1]:8080", mode: HTTPHTTPSOrSOCKS5, port: 8080},
	} {
		t.Run(test.raw, func(t *testing.T) {
			normalized, port, err := Normalize(test.raw, test.mode)
			if err != nil {
				t.Fatal(err)
			}
			if normalized == "" || port != test.port {
				t.Fatalf("Normalize(%q) = %q, %d", test.raw, normalized, port)
			}
		})
	}
}

func TestNormalizeRejectsUnsafeProxyAuthorities(t *testing.T) {
	for _, test := range []struct {
		raw  string
		mode Mode
		err  error
	}{
		{raw: "http://user:password@proxy.example.test", mode: HTTPOrHTTPS, err: ErrInvalidAuthority},
		{raw: "http://@proxy.example.test", mode: HTTPOrHTTPS, err: ErrInvalidAuthority},
		{raw: "http:proxy.example.test", mode: HTTPOrHTTPS, err: ErrInvalidAuthority},
		{raw: "http://proxy.example.test/path", mode: HTTPOrHTTPS, err: ErrInvalidAuthority},
		{raw: "http://proxy.example.test?token=secret", mode: HTTPOrHTTPS, err: ErrInvalidAuthority},
		{raw: "socks5://proxy.example.test:1080", mode: HTTPOrHTTPS, err: ErrUnsupportedScheme},
		{raw: "socks5h://proxy.example.test:1080", mode: HTTPHTTPSOrSOCKS5, err: ErrUnsupportedScheme},
		{raw: "socks5://proxy.example.test", mode: HTTPHTTPSOrSOCKS5, err: ErrSOCKS5Port},
		{raw: "http://proxy.example.test:0", mode: HTTPOrHTTPS, err: ErrInvalidPort},
		{raw: "http://bad_host.example.test", mode: HTTPOrHTTPS, err: ErrInvalidHost},
	} {
		t.Run(test.raw, func(t *testing.T) {
			if _, _, err := Normalize(test.raw, test.mode); !errors.Is(err, test.err) {
				t.Fatalf("Normalize(%q) error = %v, want %v", test.raw, err, test.err)
			}
		})
	}
}

func TestNormalizeNoProxyRejectsBypassAndUnboundedValues(t *testing.T) {
	for _, value := range []string{
		"*",
		"localhost,,.svc",
		"localhost," + strings.Repeat("a", 254),
		strings.Repeat("host.example.test,", 64) + "last.example.test",
	} {
		if _, err := NormalizeNoProxy(value); !errors.Is(err, ErrInvalidNoProxy) {
			t.Fatalf("NormalizeNoProxy(%q) error = %v", value, err)
		}
	}
	normalized, err := NormalizeNoProxy(" 127.0.0.1, localhost ,.svc ")
	if err != nil || normalized != "127.0.0.1,localhost,.svc" {
		t.Fatalf("NormalizeNoProxy returned %q, %v", normalized, err)
	}
}
