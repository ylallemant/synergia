package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"
)

type Config struct {
	AdminUser        string
	AdminPassword    string
	OIDCEnabled      bool
	OIDCClientID     string
	OIDCClientSecret string
	OIDCProviderURL  string
	OIDCRedirectURL  string
	Insecure         bool // when true, session cookies are not marked Secure (HTTP dev mode)
}

type Auth struct {
	Config       Config
	sessions     map[string]time.Time // sessionID -> expiry
	sessionsMu   sync.RWMutex
	oidcStates   map[string]time.Time // CSRF state -> expiry
	oidcStatesMu sync.RWMutex
	oauth2Config *oauth2.Config
	verifier     *oidc.IDTokenVerifier
}

func New(config Config) (*Auth, error) {
	a := &Auth{
		Config:     config,
		sessions:   make(map[string]time.Time),
		oidcStates: make(map[string]time.Time),
	}

	if config.OIDCEnabled {
		provider, err := oidc.NewProvider(context.Background(), config.OIDCProviderURL)
		if err != nil {
			return nil, fmt.Errorf("failed to create OIDC provider: %w", err)
		}

		a.oauth2Config = &oauth2.Config{
			ClientID:     config.OIDCClientID,
			ClientSecret: config.OIDCClientSecret,
			RedirectURL:  config.OIDCRedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		}

		a.verifier = provider.Verifier(&oidc.Config{ClientID: config.OIDCClientID})
	}

	return a, nil
}

func (a *Auth) generateToken() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func (a *Auth) setSession(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Secure:   !a.Config.Insecure,
		SameSite: http.SameSiteLaxMode,
	})
	a.sessionsMu.Lock()
	a.sessions[sessionID] = time.Now().Add(24 * time.Hour)
	a.sessionsMu.Unlock()
}

func (a *Auth) getSession(r *http.Request) string {
	cookie, err := r.Cookie("session")
	if err != nil {
		return ""
	}
	sessionID := cookie.Value
	a.sessionsMu.RLock()
	expiry, exists := a.sessions[sessionID]
	a.sessionsMu.RUnlock()
	if !exists || time.Now().After(expiry) {
		return ""
	}
	return sessionID
}

func (a *Auth) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   !a.Config.Insecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (a *Auth) storeOIDCState(state string) {
	a.oidcStatesMu.Lock()
	a.oidcStates[state] = time.Now().Add(10 * time.Minute)
	a.oidcStatesMu.Unlock()
}

func (a *Auth) consumeOIDCState(state string) bool {
	a.oidcStatesMu.Lock()
	defer a.oidcStatesMu.Unlock()
	expiry, ok := a.oidcStates[state]
	if !ok || time.Now().After(expiry) {
		return false
	}
	delete(a.oidcStates, state)
	return true
}

func (a *Auth) LoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.serveLoginPage(w, r)
		return
	}
	if r.Method == http.MethodPost {
		a.handleLogin(w, r)
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (a *Auth) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	tmpl := `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Synergia Admin Login</title>
    <link rel="stylesheet" href="/static/default.css">
    <style>
        body { display:flex; align-items:center; justify-content:center; min-height:100vh; max-width:none; padding:2rem; padding-top:2rem; }
        .login-card { width:100%; max-width:360px; }
        .login-card h1 { font-size:1.4rem; margin-bottom:1.25rem; color:#1a1a1a; }
        .oidc-link { display:block; text-align:center; margin-top:1rem; font-size:0.85rem; color:#2563eb; text-decoration:none; }
        .oidc-link:hover { text-decoration:underline; }
    </style>
</head>
<body>
<div class="login-card">
    <h1>Synergia Admin</h1>
    <div class="form-section">
        <form method="post" class="admin-form">
            <div class="form-row">
                <label for="username">Username</label>
                <input type="text" id="username" name="username" required autofocus>
            </div>
            <div class="form-row">
                <label for="password">Password</label>
                <input type="password" id="password" name="password" required>
            </div>
            <div class="form-actions">
                <button type="submit">Log in</button>
            </div>
        </form>
    </div>
    {{if .OIDCEnabled}}
    <a href="/auth/oidc/login" class="oidc-link">Log in with SSO</a>
    {{end}}
</div>
</body>
</html>`
	t := template.Must(template.New("login").Parse(tmpl))
	t.Execute(w, map[string]bool{"OIDCEnabled": a.Config.OIDCEnabled})
}

func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	if username == a.Config.AdminUser && password == a.Config.AdminPassword {
		sessionID := a.generateToken()
		a.setSession(w, sessionID)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	http.Error(w, "invalid credentials", http.StatusUnauthorized)
}

func (a *Auth) OIDCLoginHandler(w http.ResponseWriter, r *http.Request) {
	if !a.Config.OIDCEnabled {
		http.Error(w, "OIDC not enabled", http.StatusNotFound)
		return
	}

	state := a.generateToken()
	a.storeOIDCState(state)
	url := a.oauth2Config.AuthCodeURL(state)
	http.Redirect(w, r, url, http.StatusFound)
}

func (a *Auth) OIDCCallbackHandler(w http.ResponseWriter, r *http.Request) {
	if !a.Config.OIDCEnabled {
		http.Error(w, "OIDC not enabled", http.StatusNotFound)
		return
	}

	state := r.URL.Query().Get("state")
	if !a.consumeOIDCState(state) {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "no code in request", http.StatusBadRequest)
		return
	}

	token, err := a.oauth2Config.Exchange(context.Background(), code)
	if err != nil {
		log.Error().Err(err).Msg("OIDC token exchange failed")
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token in token response", http.StatusUnauthorized)
		return
	}

	idToken, err := a.verifier.Verify(context.Background(), rawIDToken)
	if err != nil {
		log.Error().Err(err).Msg("ID token verification failed")
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}

	_ = idToken

	sessionID := a.generateToken()
	a.setSession(w, sessionID)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Auth) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	a.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// RequireAuth wraps a handler requiring a valid session. Unauthenticated requests
// are redirected to /login.
func (a *Auth) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.getSession(r) != "" {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// RequireAuthOrBearer wraps a handler accepting either a valid session cookie or
// a Bearer token matching apiKey. Redirects to /login only when both are absent.
// Use this for admin API routes that need to support both browser sessions and
// programmatic / automated access.
func (a *Auth) RequireAuthOrBearer(apiKey string, next http.HandlerFunc) http.HandlerFunc {
	return a.RequireAuthOrBearerFn(func() string { return apiKey }, next)
}

// RequireAuthOrBearerFn is like RequireAuthOrBearer but reads the API key via
// keyFn on every request, allowing the key to be updated at runtime.
func (a *Auth) RequireAuthOrBearerFn(keyFn func() string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.getSession(r) != "" {
			next(w, r)
			return
		}
		if r.Header.Get("Authorization") == "Bearer "+keyFn() {
			next(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}
