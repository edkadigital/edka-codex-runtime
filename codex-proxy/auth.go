package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type remoteAuthorizer struct {
	sharedSecretFile string
	issuer           string
	audience         string
}

func (a remoteAuthorizer) authorize(request *http.Request) error {
	token, err := bearerToken(request.Header.Get("Authorization"))
	if err != nil {
		return err
	}
	secret, err := os.ReadFile(a.sharedSecretFile)
	if err != nil {
		return fmt.Errorf("read WebSocket shared secret: %w", err)
	}
	secret = bytes.TrimSpace(secret)
	if len(secret) < 32 {
		return fmt.Errorf("WebSocket shared secret must contain at least 32 bytes")
	}

	parsed, err := jwt.Parse(
		token,
		func(candidate *jwt.Token) (any, error) {
			if candidate.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("unexpected signing method %q", candidate.Method.Alg())
			}
			return secret, nil
		},
		jwt.WithAudience(a.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithIssuer(a.issuer),
		jwt.WithLeeway(30*time.Second),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		return fmt.Errorf("verify remote-auth token: %w", err)
	}
	if !parsed.Valid {
		return fmt.Errorf("verify remote-auth token: token is invalid")
	}
	return nil
}

func bearerToken(header string) (string, error) {
	scheme, token, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("missing bearer token")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("invalid bearer token")
	}
	return token, nil
}
