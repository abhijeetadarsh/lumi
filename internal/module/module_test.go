package module_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeetadarsh/lumi/internal/module"
	"github.com/abhijeetadarsh/lumi/modules"
)

const validSpec = `
name: english-coach
version: 9
model: { tier: reasoning, temperature: 0.3 }
system: be strict
`

func TestBuiltinModulesAreValid(t *testing.T) {
	r, err := module.Load(modules.FS, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"daily-assistant", "english-coach", "interview-coach"} {
		if _, err := r.Get(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
}

func TestParseRejectsInvalidSpecs(t *testing.T) {
	tests := map[string]string{
		"unknown field":     validSpec + "critc: true\n",
		"bad tier":          strings.Replace(validSpec, "reasoning", "turbo", 1),
		"temperature range": strings.Replace(validSpec, "0.3", "3.5", 1),
		"empty system":      strings.Replace(validSpec, "be strict", `""`, 1),
		"bad name":          strings.Replace(validSpec, "english-coach", "English Coach", 1),
		"zero version":      strings.Replace(validSpec, "version: 9", "version: 0", 1),
	}
	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := module.Parse([]byte(doc)); err == nil {
				t.Error("Parse succeeded, want error")
			}
		})
	}
}

func TestUserModuleOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "english-coach.yaml"), []byte(validSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := module.Load(modules.FS, dir)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := r.Get("english-coach")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != 9 {
		t.Errorf("version = %d, want 9 (user override)", spec.Version)
	}
	for _, e := range r.List() {
		if e.Spec.Name == "english-coach" && e.Source != module.SourceUser {
			t.Errorf("source = %q, want user", e.Source)
		}
	}
}

func TestLoadRejectsNameMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte(validSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := module.Load(modules.FS, dir); err == nil {
		t.Error("Load succeeded, want name mismatch error")
	}
}

func TestGetUnknownModule(t *testing.T) {
	r, err := module.Load(modules.FS, filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("nope"); !errors.Is(err, module.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
