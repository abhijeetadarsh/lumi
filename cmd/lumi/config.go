package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/abhijeetadarsh/lumi/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage Lumi configuration",
	}

	var force bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write a commented default config to the Lumi home directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := config.Home()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(home, 0o700); err != nil {
				return err
			}
			path := config.FilePath(home)
			flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
			if force {
				flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			}
			f, err := os.OpenFile(path, flags, 0o600)
			if errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("%s already exists (use --force to overwrite)", path)
			}
			if err != nil {
				return err
			}
			if _, err := f.Write(config.DefaultYAML()); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := config.Home()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), config.FilePath(home))
			return nil
		},
	}

	cmd.AddCommand(initCmd, pathCmd)
	return cmd
}
