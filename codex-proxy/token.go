package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Token struct {
	AccessToken string     `json:"accessToken"`
	AccessHash  string     `json:"accessTokenHash"`
	AccountID   string     `json:"chatgptAccountId"`
	PlanType    string     `json:"chatgptPlanType,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
}

type TokenBroker interface {
	Token(context.Context, string) (Token, error)
}

type HTTPBroker struct {
	URL       string
	TokenFile string
	Client    *http.Client
}

func (b HTTPBroker) Token(ctx context.Context, previousHash string) (Token, error) {
	requestBody := map[string]string{}
	if previousHash = strings.TrimSpace(previousHash); previousHash != "" {
		requestBody["previousAccessTokenHash"] = previousHash
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return Token{}, fmt.Errorf("encode broker request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL, bytes.NewReader(payload))
	if err != nil {
		return Token{}, fmt.Errorf("create broker request: %w", err)
	}
	brokerToken, err := os.ReadFile(b.TokenFile)
	if err != nil {
		return Token{}, fmt.Errorf("read broker token: %w", err)
	}
	if len(bytes.TrimSpace(brokerToken)) == 0 {
		return Token{}, fmt.Errorf("read broker token: token file is empty")
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(brokerToken)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "edka-codex-proxy/1.0")

	client := b.Client
	if client == nil {
		// Codex 0.145 gives external-auth refresh requests 10 seconds.
		// Leave headroom for the proxy to encode and forward the response.
		client = &http.Client{Timeout: 8 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return Token{}, fmt.Errorf("request subscription token: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("read broker response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Token{}, fmt.Errorf("request subscription token: broker returned HTTP %d", response.StatusCode)
	}

	var token Token
	if err := json.Unmarshal(body, &token); err != nil {
		return Token{}, fmt.Errorf("decode broker response: %w", err)
	}
	if token.AccessToken == "" || token.AccessHash == "" || token.AccountID == "" {
		return Token{}, fmt.Errorf("broker returned an incomplete subscription token")
	}
	if token.ExpiresAt == nil || !token.ExpiresAt.After(time.Now()) {
		return Token{}, fmt.Errorf("broker returned an expired subscription token")
	}
	return token, nil
}

type resilientBroker struct {
	upstream TokenBroker
	now      func() time.Time

	mu          sync.Mutex
	cached      Token
	hasCached   bool
	nextAttempt time.Time
	failures    uint
}

func newResilientBroker(upstream TokenBroker) *resilientBroker {
	return &resilientBroker{upstream: upstream, now: time.Now}
}

func (b *resilientBroker) Token(ctx context.Context, previousHash string) (Token, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	if now.Before(b.nextAttempt) && b.cachedUsable(now) {
		return b.cached, nil
	}

	token, err := b.upstream.Token(ctx, previousHash)
	if err == nil {
		b.cached = token
		b.hasCached = true
		b.failures = 0
		b.nextAttempt = time.Time{}
		return token, nil
	}

	b.failures++
	b.nextAttempt = now.Add(backoffDuration(b.failures))
	if b.cachedUsable(now) {
		return b.cached, nil
	}
	return Token{}, err
}

func (b *resilientBroker) cachedUsable(now time.Time) bool {
	return b.hasCached && b.cached.ExpiresAt != nil && b.cached.ExpiresAt.After(now.Add(30*time.Second))
}

func backoffDuration(failures uint) time.Duration {
	if failures == 0 {
		return 0
	}
	shift := min(failures-1, 5)
	return min(time.Second*time.Duration(1<<shift), 30*time.Second)
}
