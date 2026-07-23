package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestHTTPBrokerReadsMountedTokenPerRequest(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "broker-token")
	if err := os.WriteFile(tokenFile, []byte("broker-token-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, request.Header.Get("Authorization"))
		requestNumber := len(authorizations)
		mu.Unlock()

		var payload map[string]string
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if payload["previousAccessTokenHash"] != "previous-hash" {
			t.Errorf("unexpected broker payload: %#v", payload)
		}
		expiresAt := time.Now().Add(time.Hour)
		_ = json.NewEncoder(w).Encode(Token{
			AccessToken: "access",
			AccessHash:  "hash",
			AccountID:   "account",
			PlanType:    "plus",
			ExpiresAt:   &expiresAt,
		})
		if requestNumber == 1 {
			if err := os.WriteFile(tokenFile, []byte("broker-token-2\n"), 0o600); err != nil {
				t.Error(err)
			}
		}
	}))
	defer server.Close()

	broker := HTTPBroker{URL: server.URL, TokenFile: tokenFile, Client: server.Client()}
	for range 2 {
		if _, err := broker.Token(context.Background(), "previous-hash"); err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(authorizations) != 2 ||
		authorizations[0] != "Bearer broker-token-1" ||
		authorizations[1] != "Bearer broker-token-2" {
		t.Fatalf("broker did not reload its mounted token: %#v", authorizations)
	}
}

func TestResilientBrokerFallsBackToUnexpiredTokenDuringBackoff(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	calls := 0
	upstream := brokerFunc(func(_ context.Context, _ string) (Token, error) {
		calls++
		if calls == 1 {
			return Token{
				AccessToken: "access",
				AccessHash:  "hash",
				AccountID:   "account",
				ExpiresAt:   &expiresAt,
			}, nil
		}
		return Token{}, errors.New("broker unavailable")
	})
	broker := newResilientBroker(upstream)
	broker.now = func() time.Time { return now }

	if _, err := broker.Token(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	fallback, err := broker.Token(context.Background(), "hash")
	if err != nil {
		t.Fatalf("unexpired cached token was not used: %v", err)
	}
	if fallback.AccessToken != "access" {
		t.Fatalf("unexpected fallback token: %#v", fallback)
	}
	if calls != 2 {
		t.Fatalf("expected two upstream calls, got %d", calls)
	}

	if _, err := broker.Token(context.Background(), "hash"); err != nil {
		t.Fatalf("backoff did not retain the cached token: %v", err)
	}
	if calls != 2 {
		t.Fatalf("broker called upstream during backoff: %d calls", calls)
	}

	now = expiresAt
	if _, err := broker.Token(context.Background(), "hash"); err == nil {
		t.Fatal("expired cached token hid an upstream failure")
	}
}

type brokerFunc func(context.Context, string) (Token, error)

func (fn brokerFunc) Token(ctx context.Context, previousHash string) (Token, error) {
	return fn(ctx, previousHash)
}
