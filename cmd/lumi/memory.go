package main

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abhijeetadarsh/lumi/internal/app"
	"github.com/abhijeetadarsh/lumi/internal/store"
)

func newMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Inspect and edit what lumi remembers between sessions",
		Long: `Facts are learned automatically when a session ends and injected into the
next one, so lumi stops asking what it has already been told. Only distilled
facts are stored, never raw transcripts.

  lumi memory list     show stored facts, newest first
  lumi memory forget   delete one fact by id or key
  lumi memory clear    delete every fact`,
	}
	cmd.AddCommand(newMemoryListCmd(), newMemoryForgetCmd(), newMemoryClearCmd())
	return cmd
}

func newMemoryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show stored facts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			a, err := app.Open(ctx)
			if err != nil {
				return err
			}
			defer a.Close()

			// Show every fact, not just the injected subset, so this is a real
			// audit view rather than a preview of one turn.
			facts, err := a.Store.Facts(ctx, 200)
			if err != nil {
				return err
			}
			if len(facts) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No facts stored yet. They appear after a session with enough substance to learn from.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "KEY\tCONF\tUSED\tUPDATED\tFACT")
			for _, f := range facts {
				fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n",
					orDash(f.Key), f.Confidence, f.Hits,
					f.UpdatedAt.Local().Format("2006-01-02"), f.Fact)
			}
			return w.Flush()
		},
	}
}

func newMemoryForgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <id-or-key>",
		Short: "Delete one fact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			a, err := app.Open(ctx)
			if err != nil {
				return err
			}
			defer a.Close()

			if err := a.Store.ForgetFact(ctx, args[0]); err != nil {
				if err == store.ErrNotFound {
					return fmt.Errorf("no fact with id or key %q (run `lumi memory list`)", args[0])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Forgot %s\n", args[0])
			return nil
		},
	}
}

func newMemoryClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Delete every stored fact",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			a, err := app.Open(ctx)
			if err != nil {
				return err
			}
			defer a.Close()

			n, err := a.Store.CountFacts(ctx)
			if err != nil {
				return err
			}
			if n == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing stored.")
				return nil
			}
			if !confirm(cmd, fmt.Sprintf("Delete all %d stored facts? This cannot be undone.", n)) {
				fmt.Fprintln(cmd.OutOrStdout(), "Cancelled.")
				return nil
			}
			if err := clearFacts(ctx, a.Store); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %d facts.\n", n)
			return nil
		},
	}
}

// clearFacts deletes every row, using the per-fact delete so the forget path
// (and its not-found handling) stays the single code path that writes facts.
func clearFacts(ctx context.Context, s *store.Store) error {
	facts, err := s.Facts(ctx, 10000)
	if err != nil {
		return err
	}
	for _, f := range facts {
		if err := s.ForgetFact(ctx, f.ID); err != nil {
			return err
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func confirm(cmd *cobra.Command, prompt string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Fscan(cmd.InOrStdin(), &answer); err != nil {
		return false
	}
	return answer == "y" || answer == "Y" || answer == "yes"
}
