package paas

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// Manifest is the app.json shipped alongside every deploy.
type Manifest struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName reports whether name is usable as an app name / URL slug.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Validate checks and normalises the manifest.
func (m *Manifest) Validate() error {
	if !ValidName(m.Name) {
		return fmt.Errorf("invalid app name %q: must be a lowercase slug (a-z, 0-9, '-'; max 63 chars)", m.Name)
	}
	if m.Title == "" {
		m.Title = m.Name
	}
	return nil
}

// ParseManifest decodes and validates a manifest.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse app.json: %w", err)
	}
	return m, m.Validate()
}

// ReadManifest reads and validates a manifest file.
func ReadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}
