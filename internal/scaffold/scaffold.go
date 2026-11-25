// Package scaffold generates a new tinypaas app from the embedded template.
package scaffold

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed all:template
var files embed.FS

// Data fills the template's [[ ]] placeholders.
type Data struct {
	Name        string
	Title       string
	Description string
}

// TitleFromName turns "meal-picker" into "Meal Picker".
func TitleFromName(name string) string {
	words := strings.Split(name, "-")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

var funcs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
}

// Write renders the template into dir, which must not exist or be empty.
func Write(dir string, d Data) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	}
	root, _ := fs.Sub(files, "template")
	return fs.WalkDir(root, ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, strings.TrimSuffix(path, ".tmpl"))
		if e.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		src, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		t, err := template.New(path).Delims("[[", "]]").Funcs(funcs).Option("missingkey=error").Parse(string(src))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		var out bytes.Buffer
		if err := t.Execute(&out, d); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return os.WriteFile(dst, out.Bytes(), 0o644)
	})
}
