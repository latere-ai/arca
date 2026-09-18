// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package issuer

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// claimsOf reads the payload of a minted token, which is what a verifier
// sees before it checks the signature.
func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("a token has three parts, this one has %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// header reads the JOSE header, which names the algorithm and the key.
func header(t *testing.T, token string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestIssuerStub(t *testing.T) {
	t.Run("it serves discovery and a key set", func(t *testing.T) {
		s := New(t)
		for _, path := range []string{"/.well-known/openid-configuration", "/jwks"} {
			body := getJSON(t, s.URL()+path)
			if len(body) == 0 {
				t.Errorf("GET %s answered nothing", path)
			}
		}
		if got := getJSON(t, s.URL()+"/.well-known/openid-configuration")["issuer"]; got != s.URL() {
			t.Errorf("the discovery document names the issuer %v", got)
		}
	})

	t.Run("it mints any subject, with Arca's audience", func(t *testing.T) {
		s := New(t)
		claims := claimsOf(t, s.Mint(Claims{Sub: "dev"}))
		if claims["sub"] != "dev" {
			t.Errorf("sub = %v", claims["sub"])
		}
		if aud, ok := claims["aud"].([]any); !ok || len(aud) != 1 || aud[0] != DefaultAudience {
			t.Errorf("aud = %v, want %q", claims["aud"], DefaultAudience)
		}
		if claims["iss"] != s.URL() {
			t.Errorf("iss = %v", claims["iss"])
		}
		if got := header(t, s.Mint(Claims{Sub: "dev"}))["alg"]; got != "RS256" {
			t.Errorf("alg = %v, want the family's RS256", got)
		}
	})

	t.Run("it mints over HTTP, which is how make run gets a token", func(t *testing.T) {
		s := New(t)
		resp, err := http.Post(s.URL()+"/mint", "application/json", //nolint:noctx // the stub is in process and the test's own
			strings.NewReader(`{"sub":"dev","authorization_details":[{"type":"arca","actions":["file.read"]}]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var minted struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
			t.Fatal(err)
		}
		claims := claimsOf(t, minted.Token)
		if claims["sub"] != "dev" {
			t.Errorf("sub = %v", claims["sub"])
		}
		if claims["authorization_details"] == nil {
			t.Error("a narrowed personal key is minted here and nowhere else, and the details did not survive")
		}
	})

	t.Run("the age of a token is the caller's to choose", func(t *testing.T) {
		s := New(t)
		old := time.Now().Add(-48 * time.Hour).Unix()
		claims := claimsOf(t, s.Mint(Claims{Sub: "dev", Iat: old, Exp: time.Now().Add(time.Hour).Unix()}))
		if int64(claims["iat"].(float64)) != old {
			t.Errorf("iat = %v, want the age the caller asked for", claims["iat"])
		}
	})

	t.Run("es256 is the second algorithm a verifier accepts", func(t *testing.T) {
		s := New(t, WithES256())
		if got := header(t, s.Mint(Claims{Sub: "dev"}))["alg"]; got != "ES256" {
			t.Errorf("alg = %v, want ES256", got)
		}
		// The shared issuer rotates by replacement: the key set holds one
		// key, and a token signed before the rotation stops verifying. It
		// is what spec 006's start-up key-set check is pointed at.
		before := s.KID()
		s.Rotate()
		if s.KID() == before {
			t.Fatal("a rotation left the key set unchanged")
		}
		keys, ok := getJSON(t, s.JWKSURL())["keys"].([]any)
		if !ok || len(keys) != 1 {
			t.Fatalf("the key set holds %v", getJSON(t, s.JWKSURL())["keys"])
		}
	})

	t.Run("a handler without a listener serves the binary", func(t *testing.T) {
		s := NewHandler(WithIssuer("http://localhost:8081"))
		if s.URL() != "http://localhost:8081" {
			t.Errorf("URL() = %q", s.URL())
		}
		if s.Handler() == nil {
			t.Error("Handler() is nil")
		}
		if got := claimsOf(t, s.Mint(Claims{Sub: "dev"}))["iss"]; got != "http://localhost:8081" {
			t.Errorf("iss = %v", got)
		}
	})

	t.Run("the clock and the key are the caller's too", func(t *testing.T) {
		at := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
		s := New(t, WithClock(func() time.Time { return at }))
		if got := int64(claimsOf(t, s.Mint(Claims{Sub: "dev"}))["iat"].(float64)); got != at.Unix() {
			t.Errorf("iat = %d, want the stub's clock %d", got, at.Unix())
		}
		if WithKey == nil || WithRS256 == nil {
			t.Error("the shared options are not re-exported")
		}
	})
}

// getJSON reads one JSON document from the stub.
func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("GET %s answered %q", url, body)
	}
	return document
}
