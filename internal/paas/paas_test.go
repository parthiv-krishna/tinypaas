package paas

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"budget": true, "meal-picker": true, "a": true, "x1": true,
		"": false, "-a": false, "a-": false, "Budget": false, "_tinypaas": false,
		"a/b": false, "..": false, strings.Repeat("a", 64): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRewriteLocation(t *testing.T) {
	target := upstream(20000)
	for in, want := range map[string]string{
		"/login?x=1":                   "/app/login?x=1",
		"/":                            "/app/",
		"/app/already":                 "/app/already",
		"relative":                     "relative",
		"https://example.com/x":        "https://example.com/x",
		"http://127.0.0.1:20000/foo?a": "/app/foo?a",
		"//127.0.0.1:20000/foo":        "/app/foo",
		"//evil.example/foo":           "//evil.example/foo",
		"/application":                 "/app/application",
	} {
		if got := rewriteLocation(in, "/app", target); got != want {
			t.Errorf("rewriteLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewriteSetCookie(t *testing.T) {
	for in, want := range map[string]string{
		"sid=1; Path=/; HttpOnly": "sid=1; Path=/app/; HttpOnly",
		"sid=1":                   "sid=1; Path=/app/",
		"sid=1; Path=/api":        "sid=1; Path=/app/api",
		"sid=1; Path=/app/x":      "sid=1; Path=/app/x",
	} {
		if got := rewriteSetCookie(in, "/app"); got != want {
			t.Errorf("rewriteSetCookie(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripCookies(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", `a=1; tinypaas_session=secret; b="q v"`)
	stripCookies(r, cookiePrefix)
	if got := r.Header.Get("Cookie"); got != `a=1; b="q v"` {
		t.Errorf("Cookie = %q", got)
	}
}

func TestSessionSigning(t *testing.T) {
	a := &authenticator{key: []byte("0123456789abcdef0123456789abcdef")}
	tok := a.sign(User{Username: "alice"})
	var u User
	if err := a.verify(tok, &u); err != nil || u.Username != "alice" {
		t.Fatalf("verify: %v %+v", err, u)
	}
	if err := a.verify(tok[:len(tok)-2]+"xx", &u); err == nil {
		t.Fatal("tampered token verified")
	}
	if safeRedirect("//evil") != "/" || safeRedirect("https://evil") != "/" || safeRedirect("/x?y") != "/x?y" {
		t.Fatal("safeRedirect")
	}
}

// buildEchoApp compiles testdata/echoapp with the given version string.
func buildEchoApp(t *testing.T, version string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "echoapp")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+version, "-o", out, "./testdata/echoapp")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build echoapp: %v\n%s", err, b)
	}
	return out
}

func deployFile(t *testing.T, s *Server, name, bin string) error {
	t.Helper()
	f, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return s.apps.Deploy(Manifest{Name: name, Title: "Echo " + name}, f)
}

func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	v1, v2, broken := buildEchoApp(t, "v1"), buildEchoApp(t, "v2"), buildEchoApp(t, "broken")

	state := t.TempDir()
	s, err := NewServer(Config{StateDir: state, PublicURL: "https://apps.example.com", PortBase: 23400, NoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.apps.StopAll()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	get := func(path string, hdr ...string) (*http.Response, map[string]string) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var m map[string]string
		_ = json.Unmarshal(body, &m)
		return resp, m
	}

	if err := deployFile(t, s, "echo", v1); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}

	// Prefix is stripped, identity + prefix headers injected, spoofed ones dropped,
	// our own cookies hidden from the app.
	resp, m := get("/echo/hello?q=1", "X-Forwarded-User", "mallory", "Cookie", "tinypaas_session=x; keep=1")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	want := map[string]string{
		"version": "v1", "path": "/hello", "query": "q=1", "user": "dev",
		"prefix": "/echo", "proto": "https", "cookie": "keep=1", "base": "/echo",
		"cwd": filepath.Join(state, "data", "echo"),
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(state, "data", "echo", "app.db")); string(b) != "v1" {
		t.Errorf("app.db = %q", b)
	}

	// /name redirects to /name/; app redirects and cookies get the prefix.
	if resp, _ := get("/echo?a=b"); resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "/echo/?a=b" {
		t.Errorf("slash redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = get("/echo/redirect")
	if loc := resp.Header.Get("Location"); loc != "/echo/login?x=1" {
		t.Errorf("Location = %q", loc)
	}
	if c := resp.Header.Get("Set-Cookie"); !strings.Contains(c, "Path=/echo/") {
		t.Errorf("Set-Cookie = %q", c)
	}

	// Unknown apps 404; homepage lists the app.
	if resp, _ := get("/nope/"); resp.StatusCode != 404 {
		t.Errorf("unknown app status %d", resp.StatusCode)
	}
	hr, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	home, _ := io.ReadAll(hr.Body)
	hr.Body.Close()
	if !bytes.Contains(home, []byte("Echo echo")) || !bytes.Contains(home, []byte(`href="/echo/"`)) {
		t.Errorf("homepage missing app:\n%s", home)
	}

	// Upgrade, then a broken release is rejected and v2 stays live.
	if err := deployFile(t, s, "echo", v2); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}
	if _, m := get("/echo/"); m["version"] != "v2" {
		t.Errorf("after v2 deploy version = %q", m["version"])
	}
	if err := deployFile(t, s, "echo", broken); err == nil || !strings.Contains(err.Error(), "previous release restored") {
		t.Fatalf("broken deploy: %v", err)
	}
	if _, m := get("/echo/"); m["version"] != "v2" {
		t.Errorf("after broken deploy version = %q", m["version"])
	}

	// Rollback swaps v2 <-> v1.
	if err := s.apps.Rollback("echo"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, m := get("/echo/"); m["version"] != "v1" {
		t.Errorf("after rollback version = %q", m["version"])
	}

	// Restart of the daemon picks apps back up from disk.
	s.apps.StopAll()
	s2, err := NewServer(Config{StateDir: state, PublicURL: "https://apps.example.com", PortBase: 23400, NoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	s2.apps.LoadAll()
	defer s2.apps.StopAll()
	a, _ := s2.apps.get("echo")
	if err := a.proc.waitHealthy(10 * time.Second); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Remove keeps data unless purged.
	if err := s2.apps.Remove("echo", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "data", "echo", "app.db")); err != nil {
		t.Errorf("data removed without purge: %v", err)
	}
}
