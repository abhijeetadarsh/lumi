package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var debug bool
	root := &cobra.Command{
		Use:          "lumi",
		Short:        "Lumi - personal, model-agnostic assistant harness",
		SilenceUsage: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			level := slog.LevelWarn
			if debug {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().BoolVar(&debug, "debug", false, "enable debug logging to stderr")
	root.AddCommand(newChatCmd(), newEvalCmd(), newKeyCmd(), newModuleCmd(), newConfigCmd(), newMemoryCmd())
	return root
}
