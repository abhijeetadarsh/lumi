// Package secrets stores provider API keys in the OS credential store.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

const service = "lumi"

// EnvVar is the Lumi-namespaced environment override for a provider key: LUMI_<PROVIDER>_API_KEY.
func EnvVar(provider string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, provider)
	return "LUMI_" + name + "_API_KEY"
}

// APIKey looks up LUMI_<PROVIDER>_API_KEY, then the provider's configured api_key_env, then the OS keyring.
func APIKey(provider, providerEnv string) (string, error) {
	envVars := []string{EnvVar(provider)}
	if providerEnv != "" {
		envVars = append(envVars, providerEnv)
	}
	for _, name := range envVars {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
	}
	v, err := keyring.Get(service, provider)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", fmt.Errorf("no API key for provider %q: run `lumi key set %s` or set %s",
			provider, provider, strings.Join(envVars, " or "))
	}
	if err != nil {
		return "", fmt.Errorf("read keyring: %w", err)
	}
	return v, nil
}

func SetAPIKey(provider, key string) error {
	if err := keyring.Set(service, provider, key); err != nil {
		return fmt.Errorf("write keyring: %w", err)
	}
	return nil
}

func DeleteAPIKey(provider string) error {
	err := keyring.Delete(service, provider)
	if errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("no API key stored for provider %q", provider)
	}
	if err != nil {
		return fmt.Errorf("delete from keyring: %w", err)
	}
	return nil
}
