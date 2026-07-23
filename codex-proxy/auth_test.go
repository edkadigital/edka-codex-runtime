package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestRemoteAuthorizerRequiresValidEnvironmentToken(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	secret := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(secretFile, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	authorizer := remoteAuthorizer{
		sharedSecretFile: secretFile,
		issuer:           "edka",
		audience:         "environment-1",
	}

	request := httptest.NewRequest("GET", "http://example.test/", nil)
	if err := authorizer.authorize(request); err == nil {
		t.Fatal("missing bearer token was accepted")
	}

	request.Header.Set("Authorization", "Bearer "+signRemoteToken(
		t,
		secret,
		"edka",
		"environment-1",
		time.Now().Add(time.Hour),
	))
	if err := authorizer.authorize(request); err != nil {
		t.Fatalf("valid bearer token was rejected: %v", err)
	}

	request.Header.Set("Authorization", "Bearer "+signRemoteToken(
		t,
		secret,
		"edka",
		"another-environment",
		time.Now().Add(time.Hour),
	))
	if err := authorizer.authorize(request); err == nil {
		t.Fatal("wrong-audience bearer token was accepted")
	}

	request.Header.Set("Authorization", "Bearer "+signRemoteToken(
		t,
		secret,
		"edka",
		"environment-1",
		time.Now().Add(-time.Minute),
	))
	if err := authorizer.authorize(request); err == nil {
		t.Fatal("expired bearer token was accepted")
	}
}

func TestRemoteAuthorizerReadsRotatedSecretPerConnection(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	oldSecret := []byte("0123456789abcdef0123456789abcdef")
	newSecret := []byte("abcdef0123456789abcdef0123456789")
	if err := os.WriteFile(secretFile, oldSecret, 0o600); err != nil {
		t.Fatal(err)
	}
	authorizer := remoteAuthorizer{
		sharedSecretFile: secretFile,
		issuer:           "edka",
		audience:         "environment-1",
	}
	oldToken := signRemoteToken(t, oldSecret, "edka", "environment-1", time.Now().Add(time.Hour))

	request := httptest.NewRequest("GET", "http://example.test/", nil)
	request.Header.Set("Authorization", "Bearer "+oldToken)
	if err := authorizer.authorize(request); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(secretFile, newSecret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.authorize(request); err == nil {
		t.Fatal("token signed with the rotated-out secret was accepted")
	}
	request.Header.Set(
		"Authorization",
		"Bearer "+signRemoteToken(t, newSecret, "edka", "environment-1", time.Now().Add(time.Hour)),
	)
	if err := authorizer.authorize(request); err != nil {
		t.Fatalf("token signed with the rotated secret was rejected: %v", err)
	}
}

func signRemoteToken(
	t *testing.T,
	secret []byte,
	issuer string,
	audience string,
	expiresAt time.Time,
) string {
	t.Helper()
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer:    issuer,
		Audience:  jwt.ClaimStrings{audience},
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		IssuedAt:  jwt.NewNumericDate(now),
	})
	signed, err := token.SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
