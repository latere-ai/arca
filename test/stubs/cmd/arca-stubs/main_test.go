// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/arca/test/stubs/authorizer"
)

// syncBuffer lets the test read stdout while run still writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var listening = regexp.MustCompile(`(\w+) listening on (\S+)`)

// start runs the binary on loopback ports and answers the address of each
// stub and a stop function returning the exit code.
func start(t *testing.T, args ...string) (addresses map[string]string, stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var out syncBuffer
	var errOut bytes.Buffer
	code := make(chan int, 1)
	args = append([]string{"-issuer-listen", "127.0.0.1:0", "-authorizer-listen", "127.0.0.1:0"}, args...)
	go func() { code <- run(ctx, args, &out, &errOut) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		found := listening.FindAllStringSubmatch(out.String(), -1)
		if len(found) == 2 {
			addresses = map[string]string{}
			for _, m := range found {
				addresses[m[1]] = "http://" + m[2]
			}
			return addresses, func() int {
				cancel()
				select {
				case c := <-code:
					return c
				case <-time.After(10 * time.Second):
					t.Fatal("the stubs did not stop")
					return -1
				}
			}
		}
		select {
		case c := <-code:
			t.Fatalf("the stubs exited %d before listening; stderr %q", c, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stubs never reported their listeners; stdout %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// post sends one request and answers the status and the decoded body.
func post(t *testing.T, url, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the stub did not answer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	answer := map[string]any{}
	_ = json.Unmarshal(raw, &answer)
	return resp.StatusCode, answer
}

// question is the body of one decision request.
const question = `{"subject":"https://issuer.example|dev","action":"file.write","resource":{"Kind":"Space","ID":"space-1"}}`

func TestTheBinaryServesBothStubs(t *testing.T) {
	addresses, stop := start(t)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("exit %d", code)
		}
	}()

	status, minted := post(t, addresses["issuer"]+"/mint", `{"sub":"dev"}`, nil)
	if status != http.StatusOK || minted["token"] == nil {
		t.Fatalf("the issuer answered %d %v", status, minted)
	}
	status, answer := post(t, addresses["authorizer"], question,
		map[string]string{"Authorization": "Bearer " + authorizer.DefaultToken})
	if status != http.StatusOK || answer["allow"] != true {
		t.Fatalf("the authorizer answered %d %v", status, answer)
	}
}

func TestTheFlagsReachTheStubs(t *testing.T) {
	addresses, stop := start(t,
		"-alg", "es256",
		"-deny", "file.write",
		"-ttl", "30",
		"-limits", `{"quota_bytes":1024}`,
		"-authorizer-token", "another-token",
	)
	defer stop()

	_, minted := post(t, addresses["issuer"]+"/mint", `{"sub":"dev"}`, nil)
	token, _ := minted["token"].(string)
	header, err := header(token)
	if err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "ES256" {
		t.Errorf("the issuer signs with %v", header["alg"])
	}

	bearer := map[string]string{"Authorization": "Bearer another-token"}
	if status, answer := post(t, addresses["authorizer"], question, bearer); status != http.StatusOK || answer["allow"] != false {
		t.Fatalf("the denied action answered %d %v", status, answer)
	}
	read := strings.Replace(question, "file.write", "file.read", 1)
	_, answer := post(t, addresses["authorizer"], read, bearer)
	if answer["allow"] != true || answer["ttl"] != float64(30) {
		t.Fatalf("an allowed action answered %v", answer)
	}
	limits, ok := answer["limits"].(map[string]any)
	if !ok || limits["quota_bytes"] != float64(1024) {
		t.Fatalf("the limits are %v", answer["limits"])
	}
	if status, _ := post(t, addresses["authorizer"], question, nil); status != http.StatusUnauthorized {
		t.Fatalf("the default bearer was still accepted: %d", status)
	}
}

func TestEveryOutageModeReachesTheAuthorizer(t *testing.T) {
	for _, mode := range []string{"malformed", "no-allow", "status:502"} {
		t.Run(mode, func(t *testing.T) {
			addresses, stop := start(t, "-fail-mode", mode)
			defer stop()
			bearer := map[string]string{"Authorization": "Bearer " + authorizer.DefaultToken}
			status, answer := post(t, addresses["authorizer"], question, bearer)
			switch mode {
			case "status:502":
				if status != http.StatusBadGateway {
					t.Fatalf("the status mode answered %d", status)
				}
			default:
				if answer["allow"] != nil {
					t.Fatalf("%s carried a verdict: %v", mode, answer)
				}
			}
		})
	}
	t.Run("conn-drop", func(t *testing.T) {
		addresses, stop := start(t, "-fail-mode", "conn-drop")
		defer stop()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, addresses["authorizer"], strings.NewReader(question))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+authorizer.DefaultToken)
		resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the connection closed before a response line
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("the dropped connection answered")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		addresses, stop := start(t, "-fail-mode", "timeout")
		defer stop()
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, addresses["authorizer"], strings.NewReader(question))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+authorizer.DefaultToken)
		resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the stub never answers
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("the hung authorizer answered")
		}
	})
}

func TestAFlagTheStubsCannotReadIsRefused(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
	}{
		// A flag the set does not know, and a value it cannot read, are
		// usage errors; a value that parses and then fails to build a stub
		// is a start-up failure.
		{[]string{"-no-such-flag"}, 2},
		{[]string{"-alg", "hs256"}, 2},
		{[]string{"-fail-mode", "sideways"}, 1},
		{[]string{"-fail-mode", "status:99"}, 1},
		{[]string{"-limits", "{not json"}, 1},
		{[]string{"-filter", "{not json"}, 1},
	} {
		var errOut bytes.Buffer
		if code := run(t.Context(), c.args, io.Discard, &errOut); code != c.code {
			t.Errorf("%v exited %d, want %d; stderr %q", c.args, code, c.code, errOut.String())
		}
	}
}

func TestAnOccupiedAddressIsAStartUpFailure(t *testing.T) {
	addresses, stop := start(t)
	defer stop()
	occupied := strings.TrimPrefix(addresses["issuer"], "http://")
	var errOut bytes.Buffer
	code := run(t.Context(), []string{"-issuer-listen", occupied, "-authorizer-listen", "127.0.0.1:0"}, io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "issuer") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	code = run(t.Context(), []string{"-issuer-listen", "127.0.0.1:0", "-authorizer-listen", occupied}, io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "authorizer") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
}

// header reads the JOSE header of a token.
func header(token string) (map[string]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		return nil, err
	}
	var h map[string]any
	return h, json.Unmarshal(raw, &h)
}
