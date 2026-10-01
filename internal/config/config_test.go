package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/abhijeetadarsh/lumi/internal/config"
)

const baseConfig = `
providers:
  acme:
    kind: openai-compatible
    base_url: https://api.acme.test/v1
    api_key_env: ACME_KEY
tiers:
  fast:      { provider: acme, model: small }
  reasoning: { provider: acme, model: large, params: { reasoning_effort: high } }
  cheap:     { provider: acme, model: small }
`

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.WriteFile(config.FilePath(home), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadWithoutProvidersExplainsWhatToDo(t *testing.T) {
	_, err := config.Load(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no providers configured") {
		t.Errorf("err = %v, want 'no providers configured'", err)
	}
}

func TestVoiceConfig(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, baseConfig+`
voice:
  stt: { provider: acme, model: listen, params: { language: en } }
  tts: { provider: acme, model: speak, voice: v1, max_chars: 200 }
`)
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Voice.STT.Model != "listen" || cfg.Voice.STT.Params["language"] != "en" || cfg.Voice.TTS.MaxChars != 200 {
		t.Errorf("voice = %+v", cfg.Voice)
	}

	writeConfig(t, home, baseConfig+"voice:\n  stt: { provider: nope }\n")
	_, err = config.Load(home)
	if err == nil || !strings.Contains(err.Error(), `voice.stt.provider "nope"`) || !strings.Contains(err.Error(), "voice.stt.model is required") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadUserProviders(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, baseConfig)
	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModule != "daily-assistant" || cfg.HistoryTurns != 20 {
		t.Errorf("defaults not applied: default_module=%q history_turns=%d", cfg.DefaultModule, cfg.HistoryTurns)
	}
	want := config.Provider{Kind: "openai-compatible", BaseURL: "https://api.acme.test/v1", APIKeyEnv: "ACME_KEY"}
	if diff := cmp.Diff(want, cfg.Providers["acme"]); diff != "" {
		t.Errorf("provider (-want +got):\n%s", diff)
	}
	if got := cfg.Tiers["reasoning"].Params["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning params = %v", cfg.Tiers["reasoning"].Params)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, baseConfig)
	t.Setenv("LUMI_DEFAULT_MODULE", "english-coach")
	t.Setenv("LUMI_HISTORY_TURNS", "7")
	t.Setenv("LUMI_TIERS__CHEAP__MODEL", "tiny")
	t.Setenv("LUMI_ACME_API_KEY", "sk-must-not-leak")

	cfg, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultModule != "english-coach" || cfg.HistoryTurns != 7 || cfg.Tiers["cheap"].Model != "tiny" {
		t.Errorf("got default_module=%q history_turns=%d cheap=%q", cfg.DefaultModule, cfg.HistoryTurns, cfg.Tiers["cheap"].Model)
	}
	if cfg.Tiers["fast"].Model != "small" {
		t.Errorf("unrelated tier changed: %+v", cfg.Tiers["fast"])
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := map[string]string{
		"unknown provider": strings.Replace(baseConfig, "fast:      { provider: acme", "fast:      { provider: nope", 1),
		"missing kind":     strings.Replace(baseConfig, "kind: openai-compatible", "", 1),
		"bad base_url":     strings.Replace(baseConfig, "https://api.acme.test/v1", "api.acme.test", 1),
		"bad api_key_env":  strings.Replace(baseConfig, "ACME_KEY", "ACME-KEY", 1),
		"missing tier":     strings.Replace(baseConfig, "  cheap:     { provider: acme, model: small }\n", "", 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeConfig(t, home, body)
			if _, err := config.Load(home); err == nil {
				t.Error("Load succeeded, want error")
			}
		})
	}
}

func TestHomeHonoursEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LUMI_HOME", dir)
	got, err := config.Home()
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.Abs(dir); got != want {
		t.Errorf("Home() = %q, want %q", got, want)
	}
}
