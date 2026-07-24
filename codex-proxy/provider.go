package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
)

type AuthMode string

const (
	AuthModeSubscription     AuthMode = "subscription"
	AuthModeOpenAIAPIKey     AuthMode = "openai_api_key"
	AuthModeOpenRouterAPIKey AuthMode = "openrouter_api_key"

	providerListenAddr = "127.0.0.1:4502"
	openAIUpstream     = "https://api.openai.com/v1"
	openRouterUpstream = "https://openrouter.ai/api/v1"
)

func (mode *AuthMode) Set(value string) error {
	candidate := AuthMode(value)
	if !candidate.Valid() {
		return fmt.Errorf("unsupported authentication mode %q", value)
	}
	*mode = candidate
	return nil
}

func (mode AuthMode) String() string {
	return string(mode)
}

func (mode AuthMode) Valid() bool {
	return mode == AuthModeSubscription ||
		mode == AuthModeOpenAIAPIKey ||
		mode == AuthModeOpenRouterAPIKey
}

func (mode AuthMode) IsAPIKey() bool {
	return mode == AuthModeOpenAIAPIKey || mode == AuthModeOpenRouterAPIKey
}

func providerUpstream(mode AuthMode) *url.URL {
	rawURL := openAIUpstream
	if mode == AuthModeOpenRouterAPIKey {
		rawURL = openRouterUpstream
	}
	upstream, err := url.Parse(rawURL)
	if err != nil {
		panic(fmt.Sprintf("parse provider upstream: %v", err))
	}
	return upstream
}

type providerTransport struct {
	tokenFile string
	upstream  http.RoundTripper
}

func (transport providerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	rawToken, err := os.ReadFile(transport.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read provider token: %w", err)
	}
	token := string(bytes.TrimSpace(rawToken))
	if token == "" {
		return nil, fmt.Errorf("read provider token: token file is empty")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("read provider token: token contains whitespace")
	}

	outbound := request.Clone(request.Context())
	outbound.Header = request.Header.Clone()
	outbound.Header.Del("Proxy-Authorization")
	outbound.Header.Set("Authorization", "Bearer "+token)

	upstream := transport.upstream
	if upstream == nil {
		upstream = http.DefaultTransport
	}
	return upstream.RoundTrip(outbound)
}

type providerProxy struct {
	reverse *httputil.ReverseProxy
}

func newProviderProxy(
	upstream *url.URL,
	tokenFile string,
	transport http.RoundTripper,
	logger *slog.Logger,
) *providerProxy {
	reverse := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme = upstream.Scheme
			request.Out.URL.Host = upstream.Host
			request.Out.URL.Path = strings.TrimSuffix(upstream.Path, "/") +
				strings.TrimPrefix(request.In.URL.Path, "/v1")
			request.Out.URL.RawPath = ""
			request.Out.Host = upstream.Host
			request.Out.Header.Del("Forwarded")
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
		},
		Transport: providerTransport{
			tokenFile: tokenFile,
			upstream:  transport,
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			logger.Error("proxy provider request", "error", err)
			http.Error(w, "provider unavailable", http.StatusBadGateway)
		},
	}
	return &providerProxy{reverse: reverse}
}

func (proxy *providerProxy) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if !allowedProviderPath(request.URL.Path) {
		http.Error(w, "provider route is not allowed", http.StatusNotFound)
		return
	}
	if !allowedProviderMethod(request.Method, request.URL.Path) {
		http.Error(w, "provider method is not allowed", http.StatusMethodNotAllowed)
		return
	}
	proxy.reverse.ServeHTTP(w, request)
}

func allowedProviderPath(requestPath string) bool {
	if path.Clean(requestPath) != requestPath {
		return false
	}
	return requestPath == "/v1/responses" ||
		strings.HasPrefix(requestPath, "/v1/responses/") ||
		requestPath == "/v1/models" ||
		strings.HasPrefix(requestPath, "/v1/models/")
}

func allowedProviderMethod(method string, requestPath string) bool {
	if requestPath == "/v1/models" || strings.HasPrefix(requestPath, "/v1/models/") {
		return method == http.MethodGet
	}
	return method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete
}
