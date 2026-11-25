// echoapp is a tiny app used by tinypaas integration tests.
package main

import (
	"encoding/json"
	"net/http"
	"os"
)

var version = "v1"

func main() {
	if os.Getenv("ECHOAPP_FAIL") != "" || version == "broken" {
		os.Exit(3)
	}
	wd, _ := os.Getwd()
	_ = os.WriteFile("app.db", []byte(version), 0o644)
	http.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc", Path: "/"})
		http.Redirect(w, r, "/login?x=1", http.StatusFound)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": version,
			"path":    r.URL.Path,
			"query":   r.URL.RawQuery,
			"cwd":     wd,
			"user":    r.Header.Get("X-Forwarded-User"),
			"groups":  r.Header.Get("X-Forwarded-Groups"),
			"prefix":  r.Header.Get("X-Forwarded-Prefix"),
			"proto":   r.Header.Get("X-Forwarded-Proto"),
			"cookie":  r.Header.Get("Cookie"),
			"base":    os.Getenv("TINYPAAS_BASE_PATH"),
		})
	})
	_ = http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil)
}
