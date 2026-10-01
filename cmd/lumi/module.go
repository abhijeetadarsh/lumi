package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/abhijeetadarsh/lumi/internal/app"
	"github.com/abhijeetadarsh/lumi/internal/config"
	"github.com/abhijeetadarsh/lumi/internal/module"
)

func newModuleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "module",
		Short: "List, inspect and validate modules",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List available modules",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				reg, err := loadModules()
				if err != nil {
					return err
				}
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "NAME\tVERSION\tTIER\tSOURCE\tDESCRIPTION")
				for _, e := range reg.List() {
					fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", e.Spec.Name, e.Spec.Version, e.Spec.Model.Tier, e.Source, e.Spec.Description)
				}
				return w.Flush()
			},
		},
		&cobra.Command{
			Use:   "show <name>",
			Short: "Print a module spec",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := loadModules()
				if err != nil {
					return err
				}
				spec, err := reg.Get(args[0])
				if err != nil {
					return err
				}
				out, err := yaml.Marshal(spec)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(out)
				return err
			},
		},
		&cobra.Command{
			Use:   "validate <file>",
			Short: "Validate a module file",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				data, err := os.ReadFile(args[0])
				if err != nil {
					return err
				}
				spec, err := module.Parse(data)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s v%d: ok\n", spec.Name, spec.Version)
				return nil
			},
		},
	)
	return cmd
}

func loadModules() (*module.Registry, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	return app.LoadModules(home)
}
