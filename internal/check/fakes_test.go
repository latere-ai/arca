// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package check

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/arca/internal/blob"
	"latere.ai/x/arca/internal/store"
)

// The stand-ins one run of the check is driven against. Every fault of spec
// 012's criterion 13 is one of these set to refuse, so the table below runs
// with no store, no cluster and no network but the test servers it starts.

// errRefused is what a dependency answers when a case made it refuse.
var errRefused = errors.New("the dependency refused")

// faultyBucket is the store of spec 003 with one call made to fail, so a
// bucket that cannot be reached and one that cannot be written under the
// prefix are two cases of one fixture.
type faultyBucket struct {
	blob.Store
	head, put, get, del error
	// wrote is what the probe key holds after the run, so a test reads
	// whether the check deleted what it wrote.
	deleted bool
}

func (b *faultyBucket) HeadBucket(ctx context.Context) error {
	if b.head != nil {
		return b.head
	}
	return b.Store.HeadBucket(ctx)
}

func (b *faultyBucket) Put(ctx context.Context, key string, body io.Reader, size int64, o blob.PutOptions) (blob.Written, error) {
	if b.put != nil {
		return blob.Written{}, b.put
	}
	return b.Store.Put(ctx, key, body, size, o)
}

func (b *faultyBucket) Get(ctx context.Context, key string) (io.ReadCloser, blob.Object, error) {
	if b.get != nil {
		return nil, blob.Object{}, b.get
	}
	return b.Store.Get(ctx, key)
}

func (b *faultyBucket) Delete(ctx context.Context, key string) error {
	if b.del != nil {
		return b.del
	}
	b.deleted = true
	return b.Store.Delete(ctx, key)
}

// bucket answers a healthy in-process store, which is the bucket a passing
// run reaches.
func bucket() *faultyBucket { return &faultyBucket{Store: blob.NewMemory()} }

// fakeDatabase is what the database requirement reaches.
type fakeDatabase struct {
	ping    error
	version string
	closed  bool
}

func (d *fakeDatabase) Ping(context.Context) error { return d.ping }
func (d *fakeDatabase) Close()                     { d.closed = true }
func (d *fakeDatabase) Querier() store.Querier     { return versionQuerier{version: d.version} }

// versionQuerier answers the one read the database line makes of the server.
type versionQuerier struct{ version string }

func (versionQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errRefused
}

func (versionQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errRefused
}

func (q versionQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return versionRow(q)
}

// versionRow scans the server version, or refuses when the case set none.
type versionRow struct{ version string }

func (r versionRow) Scan(dest ...any) error {
	if r.version == "" {
		return errRefused
	}
	if len(dest) != 1 {
		return errRefused
	}
	target, ok := dest[0].(*string)
	if !ok {
		return errRefused
	}
	*target = r.version
	return nil
}

// endpoint is an authorizer that answers every question the same way, which
// is the misconfiguration the authorizer line exists to catch: the shared
// stub denies the probe itself, so an endpoint that allows it cannot be one.
func endpoint(t *testing.T, allow bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"allow": allow, "reason": "the stub answers one way"})
	}))
	t.Cleanup(server.Close)
	return server
}

// versionEndpoint is a server answering the build identity of spec 002 at
// /version, which is what ARCA_PUBLIC_URL is held to.
func versionEndpoint(t *testing.T, body any, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server
}

// keySet is an issuer whose discovery answers and whose key set is the
// document the case wrote, for the key shapes issuertest does not serve.
func keySet(t *testing.T, document string) string {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewUnstartedServer(mux)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": "http://" + server.Listener.Addr().String() + "/jwks"})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, document)
	})
	server.Start()
	t.Cleanup(server.Close)
	return server.URL
}

// discovery is an issuer whose discovery document names the key set given,
// so a case drives a document that names none.
func discovery(t *testing.T, jwksURI string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": jwksURI})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// unreachable is an address nothing listens on, for the two cases a
// dependency that cannot be dialed at all is read differently.
func unreachable(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	return url
}
