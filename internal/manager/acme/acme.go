// Package acme centralises handling of ACME HTTP-01 challenge paths
// (`/.well-known/acme-challenge/...`) on the manager's HTTP listeners.
//
// Every listener that has either a catch-all route or an authentication
// wrapper risks intercepting ACME validation traffic and breaking
// certificate issuance (cert-manager / certbot / autocert). Two failure
// modes have been observed in production:
//
//   - The admin listener's catch-all dashboard handler returns HTTP 401
//     for unauthenticated requests, including ACME challenges.
//   - The main API listener's catch-all routes ACME requests to the
//     public community page, returning HTML with status 200.
//
// Both responses fail ACME validation. The fix is to register an
// explicit, unauthenticated handler for the well-known ACME prefix on
// every listener before the catch-all is wired up. The default behaviour
// is to return a clean 404 — this signals "no challenge token available
// here" without leaking application content or auth challenges, and lets
// the ingress / proxy layer route legitimate ACME traffic to the real
// solver (cert-manager's pod, certbot's webroot, etc.) when it is
// configured.
//
// The Go `net/http.ServeMux` selects the longest matching prefix, so
// registering `/.well-known/acme-challenge/` here always takes
// precedence over a `/` catch-all on the same mux.
package acme

import "net/http"

// ChallengePrefix is the URL prefix reserved for ACME HTTP-01 challenges
// by RFC 8555 §8.3. All paths beginning with this prefix MUST be
// reachable without authentication and MUST NOT be redirected to a
// different host or scheme.
const ChallengePrefix = "/.well-known/acme-challenge/"

// NotFoundHandler returns an http.Handler that responds with 404 to any
// request, with no body parsing or authentication. Use it as the default
// passthrough on listeners that should not serve ACME challenges
// themselves but must not intercept them with the wrong response either.
func NotFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}

// RegisterPassthrough installs a no-auth 404 handler for the ACME
// challenge prefix on the given mux. Because Go's ServeMux uses
// longest-prefix matching, this handler shadows any `/` catch-all and
// guarantees ACME traffic is never authenticated, redirected, or routed
// to application handlers.
//
// Call this on every *http.ServeMux that backs a public-facing listener
// (main API mux, admin mux, any future listener). It is idempotent only
// in the sense that re-registering on the same mux will panic — the same
// rule as any other ServeMux pattern. Register exactly once per mux.
func RegisterPassthrough(mux *http.ServeMux) {
	mux.Handle(ChallengePrefix, NotFoundHandler())
}

// WrapHandler returns a handler that intercepts ACME challenge paths and
// serves a 404 directly, delegating all other paths to next. Use this
// when the listener is not a *http.ServeMux — for example, the
// HTTP→HTTPS redirect server, which would otherwise 301-redirect ACME
// traffic to HTTPS. Per RFC 8555 §10.2 redirects are technically
// permitted, but intercepting at the redirect server is cleaner and
// avoids any ambiguity about which listener is authoritative for ACME.
func WrapHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isChallengePath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isChallengePath reports whether p is under the ACME challenge prefix.
// Exposed via WrapHandler; kept private to discourage ad-hoc usage —
// new call sites should go through RegisterPassthrough or WrapHandler so
// the rule stays in one place.
func isChallengePath(p string) bool {
	return len(p) >= len(ChallengePrefix) && p[:len(ChallengePrefix)] == ChallengePrefix
}
