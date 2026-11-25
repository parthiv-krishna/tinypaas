package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	if err := Write(dir, Data{Name: "meal-picker", Title: TitleFromName("meal-picker"), Description: "Pick meals"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"go.mod", "go.sum", "main.go", "app.json", "flake.nix", "AGENTS.md",
		".gitignore", ".forgejo/workflows/deploy.yaml", "templates/index.html", "static/style.css"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if strings.Contains(string(b), "[[") {
			t.Errorf("%s contains unrendered placeholders", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "app.json"))
	if !strings.Contains(string(b), `"title": "Meal Picker"`) {
		t.Errorf("app.json:\n%s", b)
	}
	if err := Write(dir, Data{Name: "x"}); err == nil {
		t.Error("expected error writing into non-empty dir")
	}
}
