// Package config loads layered configuration: built-in defaults, then
// <home>/config.yaml, then LUMI_* environment variables.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

//go:embed default.yaml
var defaultYAML []byte

const envPrefix = "LUMI_"

type Config struct {
	Home          string              `koanf:"-"`
	DefaultModule string              `koanf:"default_module"`
	HistoryTurns  int                 `koanf:"history_turns"`
	Providers     map[string]Provider `koanf:"providers"`
	Tiers         map[string]Tier     `koanf:"tiers"`
	Voice         Voice               `koanf:"voice"`
}

type Provider struct {
	Kind      string `koanf:"kind"` // wire protocol adapter, e.g. openai-compatible
	BaseURL   string `koanf:"base_url"`
	NoAuth    bool   `koanf:"no_auth"`
	APIKeyEnv string `koanf:"api_key_env"`
}

type Tier struct {
	Provider      string         `koanf:"provider"`
	Model         string         `koanf:"model"`
	NoTemperature bool           `koanf:"no_temperature"`
	Params        map[string]any `koanf:"params"`
}

// Voice is optional; each half is enabled by setting its provider.
type Voice struct {
	STT STT `koanf:"stt"`
	TTS TTS `koanf:"tts"`
}

type STT struct {
	Provider string         `koanf:"provider"`
	Model    string         `koanf:"model"`
	Params   map[string]any `koanf:"params"`
}

type TTS struct {
	Provider string         `koanf:"provider"`
	Model    string         `koanf:"model"`
	Voice    string         `koanf:"voice"`
	MaxChars int            `koanf:"max_chars"`
	Params   map[string]any `koanf:"params"`
}

// Home returns LUMI_HOME, or ~/.lumi when unset.
func Home() (string, error) {
	if h := os.Getenv(envPrefix + "HOME"); h != "" {
		return filepath.Abs(h)
	}
	d, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(d, ".lumi"), nil
}

func FilePath(home string) string { return filepath.Join(home, "config.yaml") }

// DefaultYAML returns the commented defaults, used as the `lumi config init` template.
func DefaultYAML() []byte { return bytes.Clone(defaultYAML) }

func Load(home string) (Config, error) {
	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(defaultYAML), yaml.Parser()); err != nil {
		return Config{}, fmt.Errorf("load default config: %w", err)
	}

	path := FilePath(home)
	switch _, err := os.Stat(path); {
	case err == nil:
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return Config{}, fmt.Errorf("load %s: %w", path, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return Config{}, fmt.Errorf("stat %s: %w", path, err)
	}

	if err := k.Load(env.Provider(".", env.Opt{Prefix: envPrefix, TransformFunc: envKey}), nil); err != nil {
		return Config{}, fmt.Errorf("load environment: %w", err)
	}

	var c Config
	if err := k.UnmarshalWithConf("", &c, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	c.Home = home
	return c, c.Validate()
}

func envKey(k, v string) (string, any) {
	k = strings.TrimPrefix(k, envPrefix)
	// LUMI_HOME locates the config itself; *_API_KEY are secrets and never enter config.
	if k == "HOME" || strings.HasSuffix(k, "_API_KEY") {
		return "", nil
	}
	return strings.ReplaceAll(strings.ToLower(k), "__", "."), v
}

func (c Config) Validate() error {
	var errs []error
	if c.DefaultModule == "" {
		errs = append(errs, errors.New("default_module is required"))
	}
	if c.HistoryTurns < 0 {
		errs = append(errs, errors.New("history_turns must be >= 0"))
	}
	if len(c.Providers) == 0 {
		errs = append(errs, fmt.Errorf("no providers configured: declare providers and tiers in %s (`lumi config init` writes a template)", FilePath(c.Home)))
		return fmt.Errorf("invalid config: %w", errors.Join(errs...))
	}

	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		p := c.Providers[n]
		if p.Kind == "" {
			errs = append(errs, fmt.Errorf("providers.%s.kind is required", n))
		}
		if u, err := url.Parse(p.BaseURL); p.BaseURL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
			errs = append(errs, fmt.Errorf("providers.%s.base_url %q must be an http(s) URL", n, p.BaseURL))
		}
		if p.APIKeyEnv != "" && !envNamePattern.MatchString(p.APIKeyEnv) {
			errs = append(errs, fmt.Errorf("providers.%s.api_key_env %q is not a valid environment variable name", n, p.APIKeyEnv))
		}
	}

	for _, t := range llm.Tiers() {
		tc, ok := c.Tiers[string(t)]
		if !ok {
			errs = append(errs, fmt.Errorf("tiers.%s is required", t))
			continue
		}
		if _, ok := c.Providers[tc.Provider]; !ok {
			errs = append(errs, fmt.Errorf("tiers.%s.provider %q is not defined in providers", t, tc.Provider))
		}
		if tc.Model == "" {
			errs = append(errs, fmt.Errorf("tiers.%s.model is required", t))
		}
	}

	for _, v := range []struct {
		key, provider, model string
		needModel            bool
	}{
		{"voice.stt", c.Voice.STT.Provider, c.Voice.STT.Model, true},
		{"voice.tts", c.Voice.TTS.Provider, c.Voice.TTS.Model, false},
	} {
		if v.provider == "" {
			continue
		}
		if _, ok := c.Providers[v.provider]; !ok {
			errs = append(errs, fmt.Errorf("%s.provider %q is not defined in providers", v.key, v.provider))
		}
		if v.needModel && v.model == "" {
			errs = append(errs, fmt.Errorf("%s.model is required", v.key))
		}
	}
	if c.Voice.TTS.MaxChars < 0 {
		errs = append(errs, errors.New("voice.tts.max_chars must be >= 0"))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	return nil
}
