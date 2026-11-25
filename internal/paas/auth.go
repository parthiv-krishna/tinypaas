package paas

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	cookiePrefix   = "tinypaas_"
	sessionCookie  = cookiePrefix + "session"
	loginCookie    = cookiePrefix + "login"
	sessionMaxAge  = 7 * 24 * time.Hour
	loginMaxAge    = 10 * time.Minute
	authPathPrefix = "/_tinypaas/"
	callbackPath   = authPathPrefix + "callback"
	loginPath      = authPathPrefix + "login"
	logoutPath     = authPathPrefix + "logout"
)

// User is the authenticated identity forwarded to apps.
type User struct {
	Username string   `json:"u"`
	Email    string   `json:"e,omitempty"`
	Name     string   `json:"n,omitempty"`
	Groups   []string `json:"g,omitempty"`
}

type userKey struct{}

func withUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

func userFrom(ctx context.Context) *User {
	u, _ := ctx.Value(userKey{}).(*User)
	return u
}

// OIDCConfig configures the OIDC relying party.
type OIDCConfig struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	AllowedGroups []string
}

// authenticator implements the OIDC login flow and signed session cookies.
type authenticator struct {
	cfg         OIDCConfig
	key         []byte
	redirectURL string
	secure      bool
	devUser     *User // when set, auth is disabled and this user is assumed

	mu       sync.Mutex
	provider *oidc.Provider
}

func (a *authenticator) oauth(ctx context.Context) (*oauth2.Config, *oidc.Provider, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider == nil {
		// Discovered lazily so apps keep serving if the IdP is down at boot.
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		p, err := oidc.NewProvider(ctx, a.cfg.Issuer)
		if err != nil {
			return nil, nil, fmt.Errorf("oidc discovery: %w", err)
		}
		a.provider = p
	}
	return &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  a.redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}, a.provider, nil
}

// require wraps h so that only authenticated users reach it.
func (a *authenticator) require(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.devUser != nil {
			h.ServeHTTP(w, r.WithContext(withUser(r.Context(), a.devUser)))
			return
		}
		var sess struct {
			User
			Exp int64 `json:"x"`
		}
		if c, err := r.Cookie(sessionCookie); err == nil && a.verify(c.Value, &sess) == nil &&
			time.Now().Unix() < sess.Exp {
			u := sess.User
			h.ServeHTTP(w, r.WithContext(withUser(r.Context(), &u)))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, loginPath+"?rd="+urlQueryEscape(r.URL.RequestURI()), http.StatusFound)
	})
}

type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Redirect string `json:"r"`
	Exp      int64  `json:"x"`
}

func (a *authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.devUser != nil {
		http.Redirect(w, r, safeRedirect(r.URL.Query().Get("rd")), http.StatusFound)
		return
	}
	conf, _, err := a.oauth(r.Context())
	if err != nil {
		log.Printf("login: %v", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	st := loginState{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		Redirect: safeRedirect(r.URL.Query().Get("rd")),
		Exp:      time.Now().Add(loginMaxAge).Unix(),
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: a.sign(st), Path: authPathPrefix,
		MaxAge: int(loginMaxAge.Seconds()), HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, conf.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (a *authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string, err error) {
		log.Printf("oidc callback: %s: %v", msg, err)
		http.Error(w, msg, status)
	}
	var st loginState
	c, err := r.Cookie(loginCookie)
	if err != nil || a.verify(c.Value, &st) != nil || time.Now().Unix() > st.Exp {
		fail(http.StatusBadRequest, "login expired, please retry", err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: authPathPrefix, MaxAge: -1})
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail(http.StatusForbidden, "login failed: "+e, errors.New(q.Get("error_description")))
		return
	}
	if q.Get("state") != st.State {
		fail(http.StatusBadRequest, "state mismatch", nil)
		return
	}
	conf, provider, err := a.oauth(r.Context())
	if err != nil {
		fail(http.StatusBadGateway, "identity provider unavailable", err)
		return
	}
	tok, err := conf.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		fail(http.StatusBadGateway, "token exchange failed", err)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := provider.Verifier(&oidc.Config{ClientID: a.cfg.ClientID}).Verify(r.Context(), rawID)
	if err != nil {
		fail(http.StatusUnauthorized, "invalid id token", err)
		return
	}
	if idt.Nonce != st.Nonce {
		fail(http.StatusUnauthorized, "nonce mismatch", nil)
		return
	}

	type claims struct {
		Sub      string   `json:"sub"`
		Username string   `json:"preferred_username"`
		Email    string   `json:"email"`
		Name     string   `json:"name"`
		Groups   []string `json:"groups"`
	}
	var cl claims
	if err := idt.Claims(&cl); err != nil {
		fail(http.StatusUnauthorized, "invalid claims", err)
		return
	}
	// Some providers (e.g. Authelia) only put profile claims in userinfo.
	if info, err := provider.UserInfo(r.Context(), oauth2.StaticTokenSource(tok)); err == nil {
		var ui claims
		if info.Claims(&ui) == nil {
			cl.Username = cmpOr(cl.Username, ui.Username)
			cl.Email = cmpOr(cl.Email, ui.Email)
			cl.Name = cmpOr(cl.Name, ui.Name)
			if len(cl.Groups) == 0 {
				cl.Groups = ui.Groups
			}
		}
	}

	if len(a.cfg.AllowedGroups) > 0 && !slices.ContainsFunc(cl.Groups, func(g string) bool {
		return slices.Contains(a.cfg.AllowedGroups, g)
	}) {
		fail(http.StatusForbidden, "you are not in a group allowed to use these apps", nil)
		return
	}

	sess := struct {
		User
		Exp int64 `json:"x"`
	}{
		User: User{Username: cmpOr(cl.Username, cl.Sub), Email: cl.Email, Name: cl.Name, Groups: cl.Groups},
		Exp:  time.Now().Add(sessionMaxAge).Unix(),
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: a.sign(sess), Path: "/",
		MaxAge: int(sessionMaxAge.Seconds()), HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, st.Redirect, http.StatusFound)
}

func (a *authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width"><title>Signed out</title>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem"><h1>Signed out</h1><p><a href="/">Sign in again</a></p>`)
}

// sign serialises v as base64(json).base64(hmac).
func (a *authenticator) sign(v any) string {
	payload, _ := json.Marshal(v)
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *authenticator) verify(token string, v any) error {
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return errors.New("malformed token")
	}
	want := hmac.New(sha256.New, a.key)
	want.Write([]byte(p))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want.Sum(nil)) {
		return errors.New("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

// loadOrCreateKey returns the session signing key, generating it on first use.
func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, os.WriteFile(path, b, 0o600)
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// safeRedirect only allows local, root-relative redirect targets.
func safeRedirect(rd string) string {
	if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") || strings.HasPrefix(rd, "/\\") {
		return "/"
	}
	return rd
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
