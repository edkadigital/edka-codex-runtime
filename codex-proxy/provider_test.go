package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestProviderProxyInjectsMountedKeyAndReloadsItPerRequest(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "provider-token")
	if err := os.WriteFile(tokenFile, []byte("provider-key-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	type receivedRequest struct {
		authorization      string
		proxyAuthorization string
		host               string
		path               string
		query              string
	}
	var mu sync.Mutex
	var received []receivedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		received = append(received, receivedRequest{
			authorization:      request.Header.Get("Authorization"),
			proxyAuthorization: request.Header.Get("Proxy-Authorization"),
			host:               request.Host,
			path:               request.URL.Path,
			query:              request.URL.RawQuery,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL + "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(newProviderProxy(
		upstreamURL,
		tokenFile,
		upstream.Client().Transport,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	))
	defer provider.Close()

	send := func(method string, providerPath string) {
		t.Helper()
		request, err := http.NewRequest(method, provider.URL+providerPath, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer non-secret-marker")
		request.Header.Set("Proxy-Authorization", "Bearer must-not-pass")
		response, err := provider.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("provider returned HTTP %d: %s", response.StatusCode, body)
		}
		if strings.Contains(string(body), "provider-key") {
			t.Fatalf("provider key leaked in response: %s", body)
		}
	}

	send(http.MethodPost, "/v1/responses?stream=true")
	if err := os.WriteFile(tokenFile, []byte("provider-key-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	send(http.MethodGet, "/v1/models")

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("expected two upstream requests, got %d", len(received))
	}
	if received[0].authorization != "Bearer provider-key-1" ||
		received[1].authorization != "Bearer provider-key-2" {
		t.Fatalf("provider did not reload and inject its mounted key: %#v", received)
	}
	for _, request := range received {
		if request.proxyAuthorization != "" {
			t.Fatalf("proxy authorization leaked upstream: %#v", request)
		}
		if request.host != upstreamURL.Host {
			t.Fatalf("untrusted host forwarded upstream: %#v", request)
		}
	}
	if received[0].path != "/api/v1/responses" || received[0].query != "stream=true" {
		t.Fatalf("unexpected Responses upstream request: %#v", received[0])
	}
	if received[1].path != "/api/v1/models" {
		t.Fatalf("unexpected models upstream request: %#v", received[1])
	}
}

func TestProviderProxyRejectsRoutesOutsideResponsesAndModels(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls++
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	provider := newProviderProxy(
		upstreamURL,
		filepath.Join(t.TempDir(), "missing-token"),
		upstream.Client().Transport,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	for _, requestPath := range []string{
		"/",
		"/v1/chat/completions",
		"/v1/files",
		"/v1/responses-extra",
		"/v1/models-extra",
		"/v1/responses/../files",
		"/v1/models//other",
	} {
		request := httptest.NewRequest(http.MethodPost, "http://provider.test"+requestPath, nil)
		recorder := httptest.NewRecorder()
		provider.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s returned HTTP %d, want %d", requestPath, recorder.Code, http.StatusNotFound)
		}
	}
	if upstreamCalls != 0 {
		t.Fatalf("disallowed provider routes reached upstream %d times", upstreamCalls)
	}
}

func TestProviderProxyRejectsMethodsOutsideProviderContract(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls++
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	provider := newProviderProxy(
		upstreamURL,
		filepath.Join(t.TempDir(), "missing-token"),
		upstream.Client().Transport,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	for _, test := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPut, path: "/v1/responses"},
		{method: http.MethodPatch, path: "/v1/responses/response-1"},
		{method: http.MethodPost, path: "/v1/models"},
		{method: http.MethodDelete, path: "/v1/models/model-1"},
	} {
		request := httptest.NewRequest(test.method, "http://provider.test"+test.path, nil)
		recorder := httptest.NewRecorder()
		provider.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf(
				"%s %s returned HTTP %d, want %d",
				test.method,
				test.path,
				recorder.Code,
				http.StatusMethodNotAllowed,
			)
		}
	}
	if upstreamCalls != 0 {
		t.Fatalf("disallowed provider methods reached upstream %d times", upstreamCalls)
	}
}

func TestProviderUpstreamIsFixedToOpenAI(t *testing.T) {
	if actual := providerUpstream().String(); actual != openAIUpstream {
		t.Errorf("provider upstream = %q, want %q", actual, openAIUpstream)
	}
}

func TestAuthModeRejectsUnsupportedProvider(t *testing.T) {
	mode := AuthModeSubscription
	if err := mode.Set("openrouter_api_key"); err == nil {
		t.Fatal("unsupported authentication mode was accepted")
	}
	if mode != AuthModeSubscription {
		t.Fatalf("rejected authentication mode changed the active mode to %q", mode)
	}
}

func TestAPIKeySettingsDoNotRequireSubscriptionBroker(t *testing.T) {
	settings := Settings{
		AuthMode:          AuthModeOpenAIAPIKey,
		Addr:              ":4500",
		Upstream:          "ws://127.0.0.1:4501",
		ProviderAddr:      "127.0.0.1:4502",
		ProviderTokenFile: "/var/run/edka/provider/token",
		SharedSecretFile:  "/var/run/edka/ws/secret",
		Issuer:            "edka",
		Audience:          "environment-1",
	}
	if err := settings.Validate(); err != nil {
		t.Errorf("OpenAI API-key mode unexpectedly required a subscription broker: %v", err)
	}
}

func TestAPIKeySettingsRequireLoopbackProviderAddress(t *testing.T) {
	settings := Settings{
		AuthMode:          AuthModeOpenAIAPIKey,
		Addr:              ":4500",
		Upstream:          "ws://127.0.0.1:4501",
		ProviderAddr:      "0.0.0.0:4502",
		ProviderTokenFile: "/var/run/edka/provider/token",
		SharedSecretFile:  "/var/run/edka/ws/secret",
		Issuer:            "edka",
		Audience:          "environment-1",
	}
	if err := settings.Validate(); err == nil ||
		!strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback provider address was accepted: %v", err)
	}
}
