package paas

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed home.html
var homeFS embed.FS

var homeTmpl = template.Must(template.ParseFS(homeFS, "home.html"))

// Config configures the daemon.
type Config struct {
	StateDir  string
	Listen    string // public HTTP listener (behind a reverse proxy)
	Socket    string // control socket path
	PublicURL string // e.g. https://apps.example.com
	PortBase  int
	OIDC      OIDCConfig
	NoAuth    bool // development only: skip login, act as user "dev"
}

// Server is the tinypaas daemon.
type Server struct {
	cfg       Config
	publicURL *url.URL
	apps      *Manager
	auth      *authenticator
}

// NewServer validates cfg and prepares the daemon.
func NewServer(cfg Config) (*Server, error) {
	pu, err := url.Parse(cfg.PublicURL)
	if err != nil || pu.Host == "" || (pu.Scheme != "http" && pu.Scheme != "https") {
		return nil, fmt.Errorf("invalid public URL %q", cfg.PublicURL)
	}
	if cfg.PortBase == 0 {
		cfg.PortBase = 20000
	}
	s := &Server{cfg: cfg, publicURL: pu}
	if s.apps, err = newManager(cfg.StateDir, cfg.PortBase, s.newAppProxy); err != nil {
		return nil, err
	}

	s.auth = &authenticator{cfg: cfg.OIDC, secure: pu.Scheme == "https"}
	s.auth.redirectURL = strings.TrimRight(pu.String(), "/") + callbackPath
	if cfg.NoAuth {
		s.auth.devUser = &User{Username: "dev", Email: "dev@localhost", Name: "Developer"}
	} else {
		if cfg.OIDC.Issuer == "" || cfg.OIDC.ClientID == "" || cfg.OIDC.ClientSecret == "" {
			return nil, errors.New("OIDC issuer, client id and client secret are required (or use --no-auth)")
		}
		if s.auth.key, err = loadOrCreateKey(filepath.Join(cfg.StateDir, "session.key")); err != nil {
			return nil, fmt.Errorf("session key: %w", err)
		}
	}
	return s, nil
}

// Handler returns the public HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+loginPath, s.auth.handleLogin)
	mux.HandleFunc("GET "+callbackPath, s.auth.handleCallback)
	mux.HandleFunc(logoutPath, s.auth.handleLogout)
	mux.HandleFunc("GET /_tinypaas/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", s.auth.require(http.HandlerFunc(s.route)))
	return mux
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		s.home(w, r)
		return
	}
	name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	a, ok := s.apps.get(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if rest == "" && !strings.HasSuffix(r.URL.Path, "/") {
		// /name -> /name/ so relative URLs in the app resolve under its prefix.
		target := "/" + name + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
		return
	}
	a.proxy.ServeHTTP(w, r)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := homeTmpl.Execute(w, map[string]any{
		"Apps": s.apps.List(),
		"User": userFrom(r.Context()),
		"Host": s.publicURL.Host,
	}); err != nil {
		log.Printf("home: %v", err)
	}
}

// controlHandler serves the management API on the unix socket.
func (s *Server) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /apps", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.apps.List())
	})
	mux.HandleFunc("POST /deploy", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Expect the manifest part first, then the binary, streamed.
		part, err := mr.NextPart()
		if err != nil || part.FormName() != "manifest" {
			http.Error(w, "expected manifest part first", http.StatusBadRequest)
			return
		}
		var man Manifest
		if err := json.NewDecoder(part).Decode(&man); err != nil {
			http.Error(w, "bad manifest: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := man.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		part, err = mr.NextPart()
		if err != nil || part.FormName() != "binary" {
			http.Error(w, "expected binary part", http.StatusBadRequest)
			return
		}
		if err := s.apps.Deploy(man, part); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		fmt.Fprintf(w, "deployed %s at %s/%s/\n", man.Name, strings.TrimRight(s.publicURL.String(), "/"), man.Name)
	})
	mux.HandleFunc("POST /apps/{name}/rollback", s.op(func(n string) error { return s.apps.Rollback(n) }))
	mux.HandleFunc("POST /apps/{name}/restart", s.op(func(n string) error { return s.apps.Restart(n) }))
	mux.HandleFunc("DELETE /apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.op(func(n string) error { return s.apps.Remove(n, r.URL.Query().Get("purge") == "1") })(w, r)
	})
	mux.HandleFunc("GET /apps/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		lines, err := s.apps.Logs(r.PathValue("name"), atoiDefault(r.URL.Query().Get("n"), 100))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
	})
	return mux
}

func (s *Server) op(fn func(name string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := fn(name); err != nil {
			status := http.StatusUnprocessableEntity
			if errors.Is(err, ErrNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		fmt.Fprintln(w, "ok")
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// Run starts all apps and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.apps.LoadAll()
	defer s.apps.StopAll()

	_ = os.Remove(s.cfg.Socket)
	ctl, err := net.Listen("unix", s.cfg.Socket)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(s.cfg.Socket, 0o660); err != nil {
		return err
	}
	pub, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}

	public := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	control := &http.Server{Handler: s.controlHandler(), ReadHeaderTimeout: 30 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- public.Serve(pub) }()
	go func() { errc <- control.Serve(ctl) }()
	log.Printf("tinypaas serving %s on %s (control: %s)", s.publicURL, s.cfg.Listen, s.cfg.Socket)

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = public.Shutdown(shutdownCtx)
	_ = control.Shutdown(shutdownCtx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }
