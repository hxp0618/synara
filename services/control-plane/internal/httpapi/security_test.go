package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestClientIPTrustsOnlyConfiguredProxyChain(t *testing.T) {
	trusted := netip.MustParsePrefix("10.0.0.0/8")
	server := &Server{config: config.Config{TrustedProxyCIDRs: []netip.Prefix{trusted}}}

	untrusted := httptest.NewRequest("GET", "/", nil)
	untrusted.RemoteAddr = "203.0.113.9:4321"
	untrusted.Header.Set("X-Forwarded-For", "192.0.2.50")
	if got := server.resolveClientIP(untrusted); got != "203.0.113.9" {
		t.Fatalf("untrusted proxy resolved client IP %q", got)
	}

	proxied := httptest.NewRequest("GET", "/", nil)
	proxied.RemoteAddr = "10.0.0.2:4321"
	proxied.Header.Set("X-Forwarded-For", "192.0.2.200, 198.51.100.7, 10.0.0.1")
	if got := server.resolveClientIP(proxied); got != "198.51.100.7" {
		t.Fatalf("trusted proxy chain resolved client IP %q", got)
	}
}

func TestSSOCallbackURLUsesConfiguredPublicURLAndRestrictsFallback(t *testing.T) {
	server := &Server{config: config.Config{PublicControlPlaneURL: "https://synara.example.com/control-plane"}}
	request := httptest.NewRequest("GET", "http://evil.example/v1/auth/sso/id/start", nil)
	request.Host = "evil.example"
	request.Header.Set("X-Forwarded-Proto", "http")
	callback, err := server.ssoCallbackURL(request, "connection-id")
	if err != nil {
		t.Fatal(err)
	}
	if callback != "https://synara.example.com/control-plane/v1/auth/sso/connection-id/callback" {
		t.Fatalf("unexpected configured callback URL %q", callback)
	}

	server.config.PublicControlPlaneURL = ""
	request = httptest.NewRequest("GET", "http://localhost:3780/v1/auth/sso/id/start", nil)
	request.RemoteAddr = "198.51.100.10:1234"
	if _, err := server.ssoCallbackURL(request, "connection-id"); err == nil {
		t.Fatal("remote request derived an SSO callback from Host")
	}

	request.RemoteAddr = "127.0.0.1:1234"
	request.Host = "localhost:3780"
	request.Header.Set("X-Forwarded-Proto", "https")
	callback, err = server.ssoCallbackURL(request, "connection-id")
	if err != nil {
		t.Fatal(err)
	}
	if callback != "http://localhost:3780/v1/auth/sso/connection-id/callback" {
		t.Fatalf("loopback callback trusted forwarded proto: %q", callback)
	}

	request.Host = "evil.example"
	if _, err := server.ssoCallbackURL(request, "connection-id"); err == nil {
		t.Fatal("loopback request derived a callback from a non-loopback Host")
	}
}

func TestSessionCookieUsesConfiguredSecurityAttributes(t *testing.T) {
	server := &Server{config: config.Config{
		CookieName: "synara_session", CookieDomain: ".example.com", CookiePath: "/control-plane",
		CookieSameSite: "strict", CookieSecure: true, SessionTTL: time.Hour,
	}}
	recorder := httptest.NewRecorder()
	server.setSessionCookie(recorder, "rotated-token")
	response := recorder.Result()
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie count = %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "synara_session" || cookie.Value != "rotated-token" || cookie.Domain != "example.com" ||
		cookie.Path != "/control-plane" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode ||
		cookie.MaxAge != 3600 || cookie.Expires.IsZero() {
		t.Fatalf("unexpected session cookie: %#v", cookie)
	}
}

func TestSecurityHeadersDisableCaching(t *testing.T) {
	server := &Server{}
	recorder := httptest.NewRecorder()
	server.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(recorder, httptest.NewRequest("GET", "/v1/auth/session", nil))
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", recorder.Header().Get("Cache-Control"))
	}
}

func TestWriteErrorRecordsStableProblemCodeForRequestMetrics(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	recorder := &responseStatusRecorder{ResponseWriter: response}
	server.writeError(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/workers/executions/test/renew", nil),
		problem.New(http.StatusConflict, "generation_fenced", "The Worker generation is obsolete."),
	)
	if recorder.status != http.StatusConflict || recorder.problemCode != "generation_fenced" {
		t.Fatalf("recorded response = status %d problem %q", recorder.status, recorder.problemCode)
	}
}
