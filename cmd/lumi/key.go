package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/abhijeetadarsh/lumi/internal/app"
	"github.com/abhijeetadarsh/lumi/internal/secrets"
)

func newKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage provider API keys in the OS credential store",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "set <provider>",
			Short: "Store an API key (prompted, or read from stdin)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				provider := args[0]
				if err := requireProvider(provider); err != nil {
					return err
				}
				key, err := readSecret(cmd, fmt.Sprintf("API key for %s: ", provider))
				if err != nil {
					return err
				}
				if key == "" {
					return errors.New("empty key")
				}
				if err := secrets.SetAPIKey(provider, key); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Stored key for %s.\n", provider)
				return nil
			},
		},
		&cobra.Command{
			Use:   "delete <provider>",
			Short: "Remove a stored API key",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := secrets.DeleteAPIKey(args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Deleted key for %s.\n", args[0])
				return nil
			},
		},
	)
	return cmd
}

func requireProvider(name string) error {
	cfg, err := app.LoadConfig()
	if err != nil {
		return err
	}
	if _, ok := cfg.Providers[name]; !ok {
		return fmt.Errorf("provider %q is not defined in config", name)
	}
	return nil
}

func readSecret(cmd *cobra.Command, prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(cmd.ErrOrStderr(), prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", fmt.Errorf("read key: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
	if err != nil {
		return "", fmt.Errorf("read key: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
