// Package app wires configuration, modules, storage, providers and the engine.
package app

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/abhijeetadarsh/lumi/internal/config"
	"github.com/abhijeetadarsh/lumi/internal/engine"
	"github.com/abhijeetadarsh/lumi/internal/module"
	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
	"github.com/abhijeetadarsh/lumi/internal/provider/openaicompat"
	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
	"github.com/abhijeetadarsh/lumi/internal/provider/systemtts"
	"github.com/abhijeetadarsh/lumi/internal/provider/googletts"
	"github.com/abhijeetadarsh/lumi/internal/secrets"
	"github.com/abhijeetadarsh/lumi/internal/store"
	"github.com/abhijeetadarsh/lumi/modules"
)

type adapter struct {
	remote bool // needs base_url and, unless no_auth, an API key
	// new returns a client implementing any of llm.LLM, speech.STT and speech.TTS;
	// each use checks for the capability it needs.
	new func(p config.Provider, apiKey string) (any, error)
}

// adapters maps a provider `kind` to its client constructor.
var adapters = map[string]adapter{
	"openai-compatible": {remote: true, new: func(p config.Provider, apiKey string) (any, error) {
		c, err := openaicompat.New(openaicompat.Options{BaseURL: p.BaseURL, APIKey: apiKey})
		if err != nil {
			return nil, err
		}
		return c, nil
	}},
	// The OS's built-in voices: offline, free, text-to-speech only.
	"system": {new: func(config.Provider, string) (any, error) { return systemtts.New(), nil }},
	"google-tts": {remote: true, new: func(p config.Provider, apiKey string) (any, error) { return googletts.New(p.BaseURL, apiKey), nil }},
}

type App struct {
	Config   config.Config
	Modules  *module.Registry
	Engine   *engine.Engine
	store    *store.Store
	resolver *resolver
}

func LoadConfig() (config.Config, error) {
	home, err := config.Home()
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, checkKinds(cfg)
}

func checkKinds(cfg config.Config) error {
	for name, p := range cfg.Providers {
		a, ok := adapters[p.Kind]
		if !ok {
			return fmt.Errorf("providers.%s.kind %q is unsupported (supported: %v)", name, p.Kind, slices.Sorted(maps.Keys(adapters)))
		}
		if a.remote && p.BaseURL == "" {
			return fmt.Errorf("providers.%s.base_url is required for kind %q", name, p.Kind)
		}
	}
	return nil
}

// LoadModules needs only the Lumi home, not a valid provider config.
func LoadModules(home string) (*module.Registry, error) {
	return module.Load(modules.FS, filepath.Join(home, "modules"))
}

func Open(ctx context.Context) (*App, error) {
	return OpenWithDB(ctx, "")
}

// OpenWithDB is Open with an explicit database path; empty means <home>/lumi.db.
func OpenWithDB(ctx context.Context, dbPath string) (*App, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	mods, err := LoadModules(cfg.Home)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", cfg.Home, err)
	}
	if dbPath == "" {
		dbPath = filepath.Join(cfg.Home, "lumi.db")
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	res := newResolver(cfg, secrets.APIKey)
	eng := engine.New(engine.Options{
		Store:        st,
		Modules:      mods,
		Resolver:     res,
		HistoryTurns: cfg.HistoryTurns,
	})
	return &App{Config: cfg, Modules: mods, Engine: eng, store: st, resolver: res}, nil
}

func (a *App) Close() error { return a.store.Close() }

// Voice resolves the configured speech clients; each is nil when not configured.
func (a *App) Voice() (*speech.STTTarget, *speech.TTSTarget, error) {
	var (
		stt *speech.STTTarget
		tts *speech.TTSTarget
	)
	if c := a.Config.Voice.STT; c.Provider != "" {
		client, err := a.resolver.client(c.Provider)
		if err != nil {
			return nil, nil, err
		}
		s, ok := client.(speech.STT)
		if !ok {
			return nil, nil, fmt.Errorf("provider %q does not support speech-to-text", c.Provider)
		}
		stt = &speech.STTTarget{STT: s, Model: c.Model, Params: c.Params}
	}
	if c := a.Config.Voice.TTS; c.Provider != "" {
		client, err := a.resolver.client(c.Provider)
		if err != nil {
			return nil, nil, err
		}
		t, ok := client.(speech.TTS)
		if !ok {
			return nil, nil, fmt.Errorf("provider %q does not support text-to-speech", c.Provider)
		}
		tts = &speech.TTSTarget{TTS: t, Model: c.Model, Voice: c.Voice, MaxChars: c.MaxChars, Params: c.Params}
	}
	return stt, tts, nil
}

// resolver builds provider clients lazily, so a missing key only matters for tiers actually used.
type resolver struct {
	cfg     config.Config
	apiKey  func(provider, providerEnv string) (string, error)
	mu      sync.Mutex
	clients map[string]any
}

func newResolver(cfg config.Config, apiKey func(provider, providerEnv string) (string, error)) *resolver {
	return &resolver{cfg: cfg, apiKey: apiKey, clients: make(map[string]any)}
}

func (r *resolver) Resolve(t llm.Tier) (llm.Target, error) {
	tc, ok := r.cfg.Tiers[string(t)]
	if !ok {
		return llm.Target{}, fmt.Errorf("tier %q is not configured", t)
	}
	client, err := r.client(tc.Provider)
	if err != nil {
		return llm.Target{}, err
	}
	l, ok := client.(llm.LLM)
	if !ok {
		return llm.Target{}, fmt.Errorf("provider %q does not support chat", tc.Provider)
	}
	return llm.Target{LLM: l, Model: tc.Model, NoTemperature: tc.NoTemperature, Params: tc.Params}, nil
}

func (r *resolver) client(name string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[name]; ok {
		return c, nil
	}
	pc, ok := r.cfg.Providers[name]
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured", name)
	}
	a, ok := adapters[pc.Kind]
	if !ok {
		return nil, fmt.Errorf("provider %q: unsupported kind %q", name, pc.Kind)
	}

	var key string
	if a.remote && !pc.NoAuth {
		k, err := r.apiKey(name, pc.APIKeyEnv)
		if err != nil {
			return nil, err
		}
		key = k
	}

	c, err := a.new(pc, key)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", name, err)
	}
	r.clients[name] = c
	return c, nil
}
