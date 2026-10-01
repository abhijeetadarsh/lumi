package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/abhijeetadarsh/lumi/internal/app"
	"github.com/abhijeetadarsh/lumi/internal/eval"
)

func newEvalCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "eval [suite.yaml...]",
		Short: "Replay golden conversations against modules and score them (default: evals/*.yaml)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				m, err := filepath.Glob(filepath.Join("evals", "*.yaml"))
				if err != nil {
					return err
				}
				args = m
			}
			if len(args) == 0 {
				return fmt.Errorf("no eval suites found (looked for evals/*.yaml)")
			}

			// Eval sessions go to a throwaway database, never the personal one.
			tmp, err := os.MkdirTemp("", "lumi-eval-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			a, err := app.OpenWithDB(cmd.Context(), filepath.Join(tmp, "eval.db"))
			if err != nil {
				return err
			}
			defer a.Close()

			out := cmd.OutOrStdout()
			passed, total := 0, 0
			for _, path := range args {
				suite, err := eval.Load(path)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "%s (%s)\n", path, suite.Module)
				results, err := eval.Run(cmd.Context(), a.Engine, suite)
				if err != nil {
					return err
				}
				for _, cr := range results {
					fmt.Fprintf(out, "  %s\n", cr.Name)
					if verbose {
						for i, t := range cr.Transcript {
							fmt.Fprintf(out, "    [%d] > %s\n        %s\n", i, t.Input, t.Reply)
							for _, s := range t.Feedback {
								fmt.Fprintf(out, "        %s: %s => %s\n", s.Kind, s.Original, s.Suggested)
							}
						}
					}
					for _, c := range cr.Checks {
						total++
						mark := "✗"
						if c.Pass {
							passed++
							mark = "✓"
						}
						fmt.Fprintf(out, "    %s %s", mark, c.Name)
						if !c.Pass {
							fmt.Fprintf(out, "  %s", c.Detail)
						}
						fmt.Fprintln(out)
					}
				}
			}
			fmt.Fprintf(out, "\n%d/%d checks passed\n", passed, total)
			if passed != total {
				return fmt.Errorf("%d check(s) failed", total-passed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "print transcripts")
	return cmd
}
