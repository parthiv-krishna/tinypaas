package paas

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// fakeIdP is a minimal OIDC provider: discovery, authorize (auto-approves),
// token (checks PKCE, signs an id_token with the nonce) and userinfo.
type fakeIdP struct {
	*httptest.Server
	key    *rsa.PrivateKey
	groups []string

	mu    sync.Mutex
	codes map[string]url.Values // code -> authorize params
}

func newFakeIdP(t *testing.T, groups []string) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key, groups: groups, codes: map[string]url.Values{}}
	mux := http.NewServeMux()
	idp.Server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.URL,
			"authorization_endpoint":                idp.URL + "/authorize",
			"token_endpoint":                        idp.URL + "/token",
			"userinfo_endpoint":                     idp.URL + "/userinfo",
			"jwks_uri":                              idp.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := randomString()
		idp.mu.Lock()
		idp.codes[code] = q
		idp.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idp.mu.Lock()
		auth, ok := idp.codes[r.Form.Get("code")]
		delete(idp.codes, r.Form.Get("code"))
		idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != auth.Get("code_challenge") {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		if id, secret, _ := r.BasicAuth(); id != "tinypaas" || secret != "s3cret" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		claims, _ := json.Marshal(map[string]any{
			"iss": idp.URL, "sub": "uid-1", "aud": "tinypaas", "nonce": auth.Get("nonce"),
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		})
		jws, _ := signer.Sign(claims)
		idToken, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "uid-1", "preferred_username": "alice", "email": "alice@example.com",
			"name": "Alice", "groups": idp.groups,
		})
	})
	t.Cleanup(idp.Close)
	return idp
}

func newOIDCServer(t *testing.T, idp *fakeIdP, allowed []string) (*Server, *httptest.Server) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	ts.Start()
	s, err := NewServer(Config{
		StateDir: t.TempDir(), PublicURL: ts.URL, PortBase: 23500,
		OIDC: OIDCConfig{Issuer: idp.URL, ClientID: "tinypaas", ClientSecret: "s3cret", AllowedGroups: allowed},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = s.Handler()
	t.Cleanup(ts.Close)
	t.Cleanup(s.apps.StopAll)
	return s, ts
}

func TestOIDCLogin(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	idp := newFakeIdP(t, []string{"admin", "family"})
	s, ts := newOIDCServer(t, idp, []string{"family"})
	if err := deployFile(t, s, "echo", buildEchoApp(t, "v1")); err != nil {
		t.Fatal(err)
	}

	// Unauthenticated: API-style requests get 401, browsers get redirected.
	resp, err := http.Post(ts.URL+"/echo/x", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST: %d", resp.StatusCode)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err = client.Get(ts.URL + "/echo/deep/path?q=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("after login: %d %s", resp.StatusCode, body)
	}
	if resp.Request.URL.Path != "/echo/deep/path" || resp.Request.URL.RawQuery != "q=1" {
		t.Errorf("not returned to original URL: %s", resp.Request.URL)
	}
	var m map[string]string
	_ = json.Unmarshal(body, &m)
	if m["user"] != "alice" || m["groups"] != "admin,family" || m["path"] != "/deep/path" {
		t.Errorf("app saw %+v", m)
	}
	if strings.Contains(m["cookie"], cookiePrefix) {
		t.Errorf("session cookie leaked to app: %q", m["cookie"])
	}

	// Session persists; the homepage greets the user.
	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Alice") {
		t.Errorf("homepage missing user")
	}

	// Tampered session cookies are rejected.
	u, _ := url.Parse(ts.URL)
	for _, c := range jar.Cookies(u) {
		if c.Name == sessionCookie {
			c.Value = strings.Replace(c.Value, "a", "b", 1) + "x"
			jar.SetCookies(u, []*http.Cookie{c})
		}
	}
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noFollow.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), loginPath) {
		t.Errorf("tampered session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestOIDCGroupRestriction(t *testing.T) {
	idp := newFakeIdP(t, []string{"guests"})
	_, ts := newOIDCServer(t, idp, []string{"family"})
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user outside allowed groups: %d", resp.StatusCode)
	}
}

func TestOIDCOpenRedirect(t *testing.T) {
	idp := newFakeIdP(t, nil)
	_, ts := newOIDCServer(t, idp, nil)
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).Get(ts.URL + loginPath + "?rd=//evil.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Host != strings.TrimPrefix(ts.URL, "http://") || resp.Request.URL.Path != "/" {
		t.Errorf("redirected to %s", resp.Request.URL)
	}
}
