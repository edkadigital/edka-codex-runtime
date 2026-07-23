package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestExternalAuthMessagesNeverContainBrokerOnlyData(t *testing.T) {
	token := Token{
		AccessToken: "access-only",
		AccessHash:  "broker-only-hash",
		AccountID:   "account-1",
		PlanType:    "plus",
	}
	login, err := loginRequest(token)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := refreshResponse(json.RawMessage(`42`), token)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{login, refresh} {
		if strings.Contains(string(payload), token.AccessHash) {
			t.Fatalf("external auth payload leaked broker-only data: %s", payload)
		}
		if !strings.Contains(string(payload), token.AccessToken) ||
			!strings.Contains(string(payload), token.AccountID) {
			t.Fatalf("external auth payload is incomplete: %s", payload)
		}
	}
}

func TestProxyAuthenticatesRemoteAndOwnsSubscriptionRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var brokerMu sync.Mutex
	var previousHashes []string
	expiresAt := time.Now().Add(time.Hour)
	broker := brokerFunc(func(_ context.Context, previousHash string) (Token, error) {
		brokerMu.Lock()
		defer brokerMu.Unlock()
		previousHashes = append(previousHashes, previousHash)
		number := len(previousHashes)
		token := Token{
			AccessToken: "access-1",
			AccessHash:  "hash-1",
			AccountID:   "account-1",
			PlanType:    "plus",
			ExpiresAt:   &expiresAt,
		}
		if number == 2 {
			token.AccessToken = "access-2"
			token.AccessHash = "hash-2"
		}
		return token, nil
	})

	upstreamErrors := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(w, request, nil)
		if err != nil {
			upstreamErrors <- err
			return
		}
		defer func() { _ = connection.CloseNow() }()

		_, initialize, err := connection.Read(ctx)
		if err != nil {
			upstreamErrors <- err
			return
		}
		if !strings.Contains(string(initialize), `"experimentalApi":true`) {
			upstreamErrors <- errors.New("proxy did not enable the experimental API")
			return
		}
		if err := connection.Write(ctx, websocket.MessageText, []byte(`{"id":1,"result":{}}`)); err != nil {
			upstreamErrors <- err
			return
		}
		if _, initialized, err := connection.Read(ctx); err != nil ||
			rpcMethod(initialized) != "initialized" {
			upstreamErrors <- errors.New("proxy did not forward the initialized notification")
			return
		}
		_, login, err := connection.Read(ctx)
		if err != nil {
			upstreamErrors <- err
			return
		}
		if rpcMethod(login) != "account/login/start" ||
			!strings.Contains(string(login), "access-1") ||
			strings.Contains(string(login), "hash-1") {
			upstreamErrors <- errors.New("proxy sent an invalid external login")
			return
		}
		if err := connection.Write(
			ctx,
			websocket.MessageText,
			[]byte(`{"id":"edka-codex-subscription-login","result":{"type":"chatgptAuthTokens"}}`),
		); err != nil {
			upstreamErrors <- err
			return
		}
		if err := connection.Write(
			ctx,
			websocket.MessageText,
			[]byte(`{"id":9,"method":"account/chatgptAuthTokens/refresh","params":{"reason":"unauthorized"}}`),
		); err != nil {
			upstreamErrors <- err
			return
		}
		_, refreshed, err := connection.Read(ctx)
		if err != nil {
			upstreamErrors <- err
			return
		}
		if !strings.Contains(string(refreshed), `"id":9`) ||
			!strings.Contains(string(refreshed), "access-2") ||
			strings.Contains(string(refreshed), "hash-2") {
			upstreamErrors <- errors.New("proxy sent an invalid external refresh response")
			return
		}
		if err := connection.Write(
			ctx,
			websocket.MessageText,
			[]byte(`{"method":"test/ready","params":{}}`),
		); err != nil {
			upstreamErrors <- err
			return
		}
		upstreamErrors <- nil
	}))
	defer upstream.Close()

	secretFile := filepath.Join(t.TempDir(), "ws-secret")
	secret := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(secretFile, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := New(Settings{
		Upstream:         strings.Replace(upstream.URL, "http://", "ws://", 1),
		SharedSecretFile: secretFile,
		Issuer:           "edka",
		Audience:         "environment-1",
		Broker:           broker,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxyServer := httptest.NewServer(http.HandlerFunc(proxy.handle))
	defer proxyServer.Close()

	headers := http.Header{}
	headers.Set(
		"Authorization",
		"Bearer "+signRemoteToken(t, secret, "edka", "environment-1", time.Now().Add(time.Hour)),
	)
	client, _, err := websocket.Dial(
		ctx,
		strings.Replace(proxyServer.URL, "http://", "ws://", 1),
		&websocket.DialOptions{HTTPHeader: headers},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.CloseNow() }()

	if err := client.Write(
		ctx,
		websocket.MessageText,
		[]byte(`{"id":1,"method":"initialize","params":{"capabilities":{}}}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, response, err := client.Read(ctx); err != nil ||
		!strings.Contains(string(response), `"id":1`) {
		t.Fatalf("read initialize response: %v: %s", err, response)
	}
	if err := client.Write(
		ctx,
		websocket.MessageText,
		[]byte(`{"method":"initialized","params":{}}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, notification, err := client.Read(ctx); err != nil ||
		rpcMethod(notification) != "test/ready" {
		t.Fatalf("read post-refresh notification: %v: %s", err, notification)
	}
	if err := <-upstreamErrors; err != nil {
		t.Fatal(err)
	}

	brokerMu.Lock()
	defer brokerMu.Unlock()
	if len(previousHashes) != 2 ||
		previousHashes[0] != "" ||
		previousHashes[1] != "hash-1" {
		t.Fatalf("unexpected broker refresh history: %#v", previousHashes)
	}
}

func TestProxyPassesThroughAPIKeyWebSocketTraffic(t *testing.T) {
	for _, authMode := range []AuthMode{AuthModeOpenAIAPIKey, AuthModeOpenRouterAPIKey} {
		t.Run(authMode.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			const initializeRequest = `{"id":1,"method":"initialize","params":{"capabilities":{"experimentalApi":false},"client":"api-mode"}}`
			const initializedNotification = `{"method":"initialized","params":{"client":"api-mode"}}`
			const refreshRequest = `{"id":9,"method":"account/chatgptAuthTokens/refresh","params":{"reason":"test"}}`
			const refreshResponsePayload = `{"id":9,"result":{"passthrough":true}}`

			upstreamErrors := make(chan error, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				connection, err := websocket.Accept(w, request, nil)
				if err != nil {
					upstreamErrors <- err
					return
				}
				defer func() { _ = connection.CloseNow() }()

				_, initialize, err := connection.Read(ctx)
				if err != nil {
					upstreamErrors <- err
					return
				}
				if string(initialize) != initializeRequest {
					upstreamErrors <- errors.New("proxy modified the API-mode initialize request")
					return
				}
				if err := connection.Write(
					ctx,
					websocket.MessageText,
					[]byte(`{"id":1,"result":{}}`),
				); err != nil {
					upstreamErrors <- err
					return
				}
				_, initialized, err := connection.Read(ctx)
				if err != nil {
					upstreamErrors <- err
					return
				}
				if string(initialized) != initializedNotification {
					upstreamErrors <- errors.New("proxy modified the API-mode initialized notification")
					return
				}
				if err := connection.Write(
					ctx,
					websocket.MessageText,
					[]byte(refreshRequest),
				); err != nil {
					upstreamErrors <- err
					return
				}
				_, refreshResponse, err := connection.Read(ctx)
				if err != nil {
					upstreamErrors <- err
					return
				}
				if string(refreshResponse) != refreshResponsePayload {
					upstreamErrors <- errors.New("proxy intercepted the API-mode refresh message")
					return
				}
				if err := connection.Write(
					ctx,
					websocket.MessageText,
					[]byte(`{"method":"test/ready","params":{}}`),
				); err != nil {
					upstreamErrors <- err
					return
				}
				upstreamErrors <- nil
			}))
			defer upstream.Close()

			secretFile := filepath.Join(t.TempDir(), "ws-secret")
			secret := []byte("0123456789abcdef0123456789abcdef")
			if err := os.WriteFile(secretFile, secret, 0o600); err != nil {
				t.Fatal(err)
			}
			brokerCalls := 0
			proxy := New(Settings{
				AuthMode:          authMode,
				Upstream:          strings.Replace(upstream.URL, "http://", "ws://", 1),
				ProviderTokenFile: filepath.Join(t.TempDir(), "provider-token"),
				SharedSecretFile:  secretFile,
				Issuer:            "edka",
				Audience:          "environment-1",
				Broker: brokerFunc(func(context.Context, string) (Token, error) {
					brokerCalls++
					return Token{}, errors.New("API mode must not call the subscription broker")
				}),
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			proxyServer := httptest.NewServer(http.HandlerFunc(proxy.handle))
			defer proxyServer.Close()

			headers := http.Header{}
			headers.Set(
				"Authorization",
				"Bearer "+signRemoteToken(
					t,
					secret,
					"edka",
					"environment-1",
					time.Now().Add(time.Hour),
				),
			)
			client, _, err := websocket.Dial(
				ctx,
				strings.Replace(proxyServer.URL, "http://", "ws://", 1),
				&websocket.DialOptions{HTTPHeader: headers},
			)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.CloseNow() }()

			if err := client.Write(ctx, websocket.MessageText, []byte(initializeRequest)); err != nil {
				t.Fatal(err)
			}
			if _, response, err := client.Read(ctx); err != nil ||
				!strings.Contains(string(response), `"id":1`) {
				t.Fatalf("read initialize response: %v: %s", err, response)
			}
			if err := client.Write(
				ctx,
				websocket.MessageText,
				[]byte(initializedNotification),
			); err != nil {
				t.Fatal(err)
			}
			if _, refresh, err := client.Read(ctx); err != nil ||
				string(refresh) != refreshRequest {
				t.Fatalf("read passthrough refresh request: %v: %s", err, refresh)
			}
			if err := client.Write(
				ctx,
				websocket.MessageText,
				[]byte(refreshResponsePayload),
			); err != nil {
				t.Fatal(err)
			}
			if _, notification, err := client.Read(ctx); err != nil ||
				rpcMethod(notification) != "test/ready" {
				t.Fatalf("read ready notification: %v: %s", err, notification)
			}
			if err := <-upstreamErrors; err != nil {
				t.Fatal(err)
			}
			if brokerCalls != 0 {
				t.Fatalf("API mode called subscription broker %d times", brokerCalls)
			}
		})
	}
}
