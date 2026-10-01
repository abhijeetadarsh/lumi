// Package module loads and validates module specs (persona + prompt + model tier).
package module

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
)

var ErrNotFound = errors.New("module not found")

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type Spec struct {
	Name        string    `yaml:"name"`
	Version     int       `yaml:"version"`
	Description string    `yaml:"description,omitempty"`
	Model       ModelSpec `yaml:"model"`
	// Feedback: the model appends a <feedback> block, delivered separately from the reply.
	Feedback bool   `yaml:"feedback,omitempty"`
	System   string `yaml:"system"`
}

type ModelSpec struct {
	Tier        llm.Tier `yaml:"tier"`
	VoiceTier   llm.Tier `yaml:"voice_tier,omitempty"`
	Temperature *float64 `yaml:"temperature,omitempty"`
	MaxTokens   int      `yaml:"max_tokens,omitempty"`
}

// Parse decodes a module strictly (unknown fields are errors) and validates it.
func Parse(data []byte) (Spec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("decode module: %w", err)
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

func (s Spec) Validate() error {
	var errs []error
	if !namePattern.MatchString(s.Name) {
		errs = append(errs, fmt.Errorf("name %q must match %s", s.Name, namePattern))
	}
	if s.Version < 1 {
		errs = append(errs, errors.New("version must be >= 1"))
	}
	if !s.Model.Tier.Valid() {
		errs = append(errs, fmt.Errorf("model.tier %q must be one of %v", s.Model.Tier, llm.Tiers()))
	}
	if s.Model.VoiceTier != "" && !s.Model.VoiceTier.Valid() {
		errs = append(errs, fmt.Errorf("model.voice_tier %q must be one of %v", s.Model.VoiceTier, llm.Tiers()))
	}
	if t := s.Model.Temperature; t != nil && (*t < 0 || *t > 2) {
		errs = append(errs, fmt.Errorf("model.temperature %v must be within [0, 2]", *t))
	}
	if s.Model.MaxTokens < 0 {
		errs = append(errs, errors.New("model.max_tokens must be >= 0"))
	}
	if strings.TrimSpace(s.System) == "" {
		errs = append(errs, errors.New("system prompt is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("module %q: %w", s.Name, err)
	}
	return nil
}

type Source string

const (
	SourceBuiltin Source = "builtin"
	SourceUser    Source = "user"
)

type Entry struct {
	Spec   Spec
	Source Source
}

type Registry struct {
	entries map[string]Entry
}

// Load reads built-in modules, then user modules from userDir, which override by name.
// A missing userDir is not an error; an invalid module file is.
func Load(builtin fs.FS, userDir string) (*Registry, error) {
	r := &Registry{entries: make(map[string]Entry)}
	if err := r.loadFS(builtin, SourceBuiltin, "builtin"); err != nil {
		return nil, err
	}
	if userDir == "" {
		return r, nil
	}
	if _, err := os.Stat(userDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return r, nil
		}
		return nil, fmt.Errorf("read module dir: %w", err)
	}
	if err := r.loadFS(os.DirFS(userDir), SourceUser, userDir); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) loadFS(fsys fs.FS, src Source, origin string) error {
	paths, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return fmt.Errorf("list modules in %s: %w", origin, err)
	}
	for _, p := range paths {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Join(origin, p), err)
		}
		spec, err := Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(origin, p), err)
		}
		if stem := strings.TrimSuffix(p, ".yaml"); stem != spec.Name {
			return fmt.Errorf("%s: module name %q must match file name %q", filepath.Join(origin, p), spec.Name, stem)
		}
		r.entries[spec.Name] = Entry{Spec: spec, Source: src}
	}
	return nil
}

func (r *Registry) Get(name string) (Spec, error) {
	e, ok := r.entries[name]
	if !ok {
		return Spec{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return e.Spec, nil
}

func (r *Registry) List() []Entry {
	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Spec.Name, b.Spec.Name) })
	return out
}
