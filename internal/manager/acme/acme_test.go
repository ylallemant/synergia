package acme

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRegisterPassthrough_ShadowsCatchAll verifies the core invariant:
// when a mux has a `/` catch-all that requires auth (the production bug
// pattern), registering the ACME passthrough must intercept challenge
// paths before the catch-all sees them.
func TestRegisterPassthrough_ShadowsCatchAll(t *testing.T) {
	mux := http.NewServeMux()

	// Simulate the admin server's auth catch-all — returns 401 for any
	// request that lands here. If the passthrough fails, ACME requests
	// would hit this and certificate issuance would break.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})

	RegisterPassthrough(mux)

	cases := []struct {
		name string
		path string
		want int
	}{
		{"challenge token", ChallengePrefix + "abc123", http.StatusNotFound},
		{"challenge prefix exact", ChallengePrefix, http.StatusNotFound},
		{"challenge with subpath", ChallengePrefix + "deep/nested/token", http.StatusNotFound},
		{"unrelated path still hits catch-all", "/dashboard", http.StatusUnauthorized},
		{"root still hits catch-all", "/", http.StatusUnauthorized},
		{"similar but different prefix", "/.well-known/other", http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("path %q: got status %d, want %d", tc.path, rec.Code, tc.want)
			}
		})
	}
}

// TestWrapHandler_InterceptsChallengePaths verifies the redirect-server
// flow: ACME paths get a clean 404, everything else falls through to
// the wrapped handler (which in production performs the HTTP→HTTPS
// 301 redirect).
func TestWrapHandler_InterceptsChallengePaths(t *testing.T) {
	// Inner handler stands in for the real redirect handler — any
	// non-ACME request should reach it.
	const innerStatus = http.StatusTeapot
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(innerStatus)
	})

	wrapped := WrapHandler(inner)

	cases := []struct {
		name string
		path string
		want int
	}{
		{"challenge intercepted", ChallengePrefix + "token", http.StatusNotFound},
		{"non-acme passes through", "/anything-else", innerStatus},
		{"root passes through", "/", innerStatus},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("path %q: got status %d, want %d", tc.path, rec.Code, tc.want)
			}
		})
	}
}

// TestRegisterPassthrough_NoAuthHeadersRequired verifies that the
// passthrough does not look at headers — ACME validators do not send
// auth headers, and the bug we are fixing is that the existing auth
// middleware rejected such requests.
func TestRegisterPassthrough_NoAuthHeadersRequired(t *testing.T) {
	mux := http.NewServeMux()
	RegisterPassthrough(mux)

	req := httptest.NewRequest(http.MethodGet, ChallengePrefix+"x", nil)
	// Deliberately no Authorization or X-API-Key header.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", rec.Code)
	}
}
