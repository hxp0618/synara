package agentd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const providerCredentialBrokerShutdownTimeout = 2 * time.Second

type providerCredentialBroker struct {
	server   *http.Server
	listener net.Listener
	done     chan error
	once     sync.Once
}

func startProviderCredentialBroker(
	ctx context.Context,
	provider string,
	credential *RunnerCredential,
) (*RunnerCredential, *providerCredentialBroker, error) {
	if credential == nil {
		return nil, nil, errors.New("Provider Credential broker requires a Credential")
	}
	configuration, err := providerCredentialBrokerConfiguration(provider, credential.Payload)
	if err != nil {
		return nil, nil, err
	}
	taskToken, err := newProviderCredentialBrokerToken()
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("listen for Provider Credential broker: %w", err)
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(configuration.upstream)
	reverseProxy.Transport, err = providerCredentialBrokerTransport(configuration.upstream)
	if err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	originalDirector := reverseProxy.Director
	reverseProxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.Host = configuration.upstream.Host
		request.Header.Del("Authorization")
		request.Header.Del("X-Api-Key")
		configuration.authorizeUpstream(request.Header)
	}
	reverseProxy.ErrorHandler = func(response http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(response, "Provider upstream is unavailable", http.StatusBadGateway)
	}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodPost &&
			request.Method != http.MethodDelete && request.Method != http.MethodHead {
			http.Error(response, "Provider method is not permitted", http.StatusMethodNotAllowed)
			return
		}
		if !providerCredentialBrokerAuthorized(request, taskToken) {
			http.Error(response, "Provider task Credential is invalid", http.StatusUnauthorized)
			return
		}
		reverseProxy.ServeHTTP(response, request)
	})
	broker := &providerCredentialBroker{
		listener: listener,
		done:     make(chan error, 1),
	}
	broker.server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() {
		err := broker.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		broker.done <- err
		close(broker.done)
	}()
	go func() {
		<-ctx.Done()
		_ = broker.Close()
	}()
	brokered := &RunnerCredential{
		GrantID: credential.GrantID,
		Access:  credential.Access,
		Payload: configuration.brokeredPayload(taskToken, "http://"+listener.Addr().String()),
	}
	return brokered, broker, nil
}

func providerCredentialBrokerTransport(upstream *url.URL) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if providerCredentialBrokerNoProxy(upstream.Hostname(), os.Getenv("SYNARA_PROVIDER_NO_PROXY")) {
		return transport, nil
	}
	proxyValue := strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_ALL_PROXY"))
	if upstream.Scheme == "https" && strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_HTTPS_PROXY")) != "" {
		proxyValue = strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_HTTPS_PROXY"))
	}
	if upstream.Scheme == "http" && strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_HTTP_PROXY")) != "" {
		proxyValue = strings.TrimSpace(os.Getenv("SYNARA_PROVIDER_HTTP_PROXY"))
	}
	if proxyValue == "" {
		return transport, nil
	}
	proxyURL, err := url.Parse(proxyValue)
	if err != nil || proxyURL.Host == "" || proxyURL.User != nil && proxyURL.User.Username() == "" ||
		(proxyURL.Scheme != "http" && proxyURL.Scheme != "https" && proxyURL.Scheme != "socks5" && proxyURL.Scheme != "socks5h") {
		return nil, errors.New("controlled Provider proxy URL is invalid")
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	return transport, nil
}

func providerCredentialBrokerNoProxy(hostname, value string) bool {
	hostname = strings.ToLower(strings.Trim(strings.TrimSpace(hostname), "[]"))
	if hostname == "" {
		return false
	}
	hostIP := net.ParseIP(hostname)
	for _, raw := range strings.Split(value, ",") {
		entry := strings.ToLower(strings.TrimSpace(raw))
		if entry == "" {
			continue
		}
		if entry == "*" || entry == hostname || strings.TrimPrefix(entry, ".") == hostname ||
			(strings.HasPrefix(entry, ".") && strings.HasSuffix(hostname, entry)) {
			return true
		}
		if _, network, err := net.ParseCIDR(entry); err == nil && hostIP != nil && network.Contains(hostIP) {
			return true
		}
	}
	return false
}

func (b *providerCredentialBroker) Close() error {
	if b == nil {
		return nil
	}
	var shutdownErr error
	b.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), providerCredentialBrokerShutdownTimeout)
		defer cancel()
		shutdownErr = b.server.Shutdown(ctx)
		if shutdownErr != nil {
			_ = b.listener.Close()
		}
		if serveErr := <-b.done; shutdownErr == nil {
			shutdownErr = serveErr
		}
	})
	return shutdownErr
}

type providerCredentialBrokerConfig struct {
	upstream          *url.URL
	authorizeUpstream func(http.Header)
	brokeredPayload   func(taskToken, baseURL string) map[string]any
}

func providerCredentialBrokerConfiguration(
	provider string,
	payload map[string]any,
) (providerCredentialBrokerConfig, error) {
	normalizedProvider := strings.ToLower(strings.TrimSpace(provider))
	baseURL, _ := payload["baseUrl"].(string)
	switch normalizedProvider {
	case "codex":
		apiKey, ok := nonEmptyProviderCredentialString(payload["apiKey"])
		if !ok {
			return providerCredentialBrokerConfig{}, errors.New("Codex Provider Credential apiKey is invalid")
		}
		upstream, err := parseProviderCredentialBrokerUpstream(baseURL, "https://api.openai.com/v1")
		if err != nil {
			return providerCredentialBrokerConfig{}, err
		}
		organization, _ := nonEmptyProviderCredentialString(payload["organization"])
		return providerCredentialBrokerConfig{
			upstream: upstream,
			authorizeUpstream: func(header http.Header) {
				header.Set("Authorization", "Bearer "+apiKey)
			},
			brokeredPayload: func(taskToken, brokerBaseURL string) map[string]any {
				result := map[string]any{"apiKey": taskToken, "baseUrl": brokerBaseURL}
				if organization != "" {
					result["organization"] = organization
				}
				return result
			},
		}, nil
	case "claude", "claudeagent":
		apiKey, hasAPIKey := nonEmptyProviderCredentialString(payload["apiKey"])
		authToken, hasAuthToken := nonEmptyProviderCredentialString(payload["authToken"])
		if hasAPIKey == hasAuthToken {
			return providerCredentialBrokerConfig{}, errors.New("Claude Provider Credential must contain exactly one secret")
		}
		upstream, err := parseProviderCredentialBrokerUpstream(baseURL, "https://api.anthropic.com")
		if err != nil {
			return providerCredentialBrokerConfig{}, err
		}
		return providerCredentialBrokerConfig{
			upstream: upstream,
			authorizeUpstream: func(header http.Header) {
				if hasAPIKey {
					header.Set("X-Api-Key", apiKey)
				} else {
					header.Set("Authorization", "Bearer "+authToken)
				}
			},
			brokeredPayload: func(taskToken, brokerBaseURL string) map[string]any {
				result := map[string]any{"baseUrl": brokerBaseURL}
				if hasAPIKey {
					result["apiKey"] = taskToken
				} else {
					result["authToken"] = taskToken
				}
				return result
			},
		}, nil
	default:
		return providerCredentialBrokerConfig{}, fmt.Errorf("Provider Credential broker does not support %q", provider)
	}
}

func parseProviderCredentialBrokerUpstream(value, fallback string) (*url.URL, error) {
	if strings.TrimSpace(value) == "" {
		value = fallback
	}
	upstream, err := url.Parse(strings.TrimSpace(value))
	if err != nil || upstream.Opaque != "" || upstream.Host == "" || upstream.User != nil || upstream.Fragment != "" ||
		(upstream.Scheme != "https" && upstream.Scheme != "http") {
		return nil, errors.New("Provider Credential baseUrl is invalid")
	}
	upstream.Path = strings.TrimRight(upstream.Path, "/")
	return upstream, nil
}

func nonEmptyProviderCredentialString(value any) (string, bool) {
	text, ok := value.(string)
	text = strings.TrimSpace(text)
	return text, ok && text != "" && !strings.ContainsAny(text, "\r\n\x00")
}

func newProviderCredentialBrokerToken() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("generate Provider task Credential: %w", err)
	}
	return "synara_task_" + base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

func providerCredentialBrokerAuthorized(request *http.Request, expected string) bool {
	provided := strings.TrimSpace(request.Header.Get("X-Api-Key"))
	if provided == "" {
		provided = strings.TrimSpace(request.Header.Get("Authorization"))
		provided = strings.TrimSpace(strings.TrimPrefix(provided, "Bearer "))
	}
	return len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
