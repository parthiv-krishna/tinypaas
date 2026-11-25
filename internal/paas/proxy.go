package paas

import (
	"fmt"
	"html"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// Identity headers set for apps. Inbound copies are always stripped so apps
// can trust them.
const (
	HeaderUser   = "X-Forwarded-User"
	HeaderEmail  = "X-Forwarded-Email"
	HeaderName   = "X-Forwarded-Name"
	HeaderGroups = "X-Forwarded-Groups"
	HeaderPrefix = "X-Forwarded-Prefix"
)

var identityHeaders = []string{HeaderUser, HeaderEmail, HeaderName, HeaderGroups, HeaderPrefix}

// newAppProxy builds the reverse proxy for one app mounted at /<name>/.
func (s *Server) newAppProxy(name string, port int) *httputil.ReverseProxy {
	prefix := "/" + name
	target := upstream(port)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = stripPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.RawPath = ""
			if pr.In.URL.RawPath != "" {
				pr.Out.URL.RawPath = stripPrefix(pr.In.URL.RawPath, prefix)
			}

			// We sit behind a trusted reverse proxy (Caddy): keep its client
			// chain and describe the public URL rather than our listener.
			if prior := pr.In.Header.Values("X-Forwarded-For"); len(prior) > 0 {
				pr.Out.Header["X-Forwarded-For"] = []string{strings.Join(prior, ", ")}
			}
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Host", s.publicURL.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", s.publicURL.Scheme)

			for _, h := range identityHeaders {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set(HeaderPrefix, prefix)
			if u := userFrom(pr.In.Context()); u != nil {
				pr.Out.Header.Set(HeaderUser, u.Username)
				if u.Email != "" {
					pr.Out.Header.Set(HeaderEmail, u.Email)
				}
				if u.Name != "" {
					pr.Out.Header.Set(HeaderName, u.Name)
				}
				if len(u.Groups) > 0 {
					pr.Out.Header.Set(HeaderGroups, strings.Join(u.Groups, ","))
				}
			}
			stripCookies(pr.Out, cookiePrefix)
		},
		ModifyResponse: func(resp *http.Response) error {
			rewriteResponse(resp, prefix, target)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[%s] proxy error: %v", name, err)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `<!doctype html><meta name="viewport" content="width=device-width"><title>%[1]s unavailable</title>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>%[1]s is not responding</h1><p>The app may be starting or crashing. Check <code>tinypaas logs %[1]s</code>.</p><p><a href="/">&larr; all apps</a></p>`,
				html.EscapeString(name))
		},
	}
}

func stripPrefix(p, prefix string) string {
	p = strings.TrimPrefix(p, prefix)
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	return p
}

// rewriteResponse maps root-relative redirects and cookie paths emitted by an
// app (which believes it lives at "/") back under its prefix.
func rewriteResponse(resp *http.Response, prefix string, target *url.URL) {
	if loc := resp.Header.Get("Location"); loc != "" {
		resp.Header.Set("Location", rewriteLocation(loc, prefix, target))
	}
	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	out := make([]string, 0, len(cookies))
	for _, raw := range cookies {
		out = append(out, rewriteSetCookie(raw, prefix))
	}
	resp.Header["Set-Cookie"] = out
}

func rewriteLocation(loc, prefix string, target *url.URL) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() || strings.HasPrefix(loc, "//") {
		if u.Host != target.Host {
			return loc // external redirect, leave alone
		}
		u.Scheme, u.Host, u.User = "", "", nil
	} else if !strings.HasPrefix(loc, "/") {
		return loc // relative redirect already resolves under the prefix
	}
	if u.Path == prefix || strings.HasPrefix(u.Path, prefix+"/") {
		return u.String() // already prefixed
	}
	u.Path = prefix + u.Path
	if u.RawPath != "" {
		u.RawPath = prefix + u.RawPath
	}
	return u.String()
}

func rewriteSetCookie(raw, prefix string) string {
	c, err := http.ParseSetCookie(raw)
	if err != nil {
		return raw
	}
	switch {
	case c.Path == "" || c.Path == "/":
		c.Path = prefix + "/"
	case strings.HasPrefix(c.Path, "/") && c.Path != prefix && !strings.HasPrefix(c.Path, prefix+"/"):
		c.Path = prefix + c.Path
	}
	if s := c.String(); s != "" {
		return s
	}
	return raw
}

// stripCookies removes cookies whose name starts with prefix from an outgoing
// request, leaving all other cookies byte-for-byte intact.
func stripCookies(r *http.Request, prefix string) {
	var keep []string
	for _, line := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if part != "" && !strings.HasPrefix(part, prefix) {
				keep = append(keep, part)
			}
		}
	}
	r.Header.Del("Cookie")
	if len(keep) > 0 {
		r.Header.Set("Cookie", strings.Join(keep, "; "))
	}
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
