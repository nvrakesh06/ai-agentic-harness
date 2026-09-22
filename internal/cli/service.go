package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/service"
	"github.com/spf13/cobra"
)

func addService(root *cobra.Command, o *options) {
	var name string
	var printOnly bool
	parent := &cobra.Command{Use: "service", Short: "Generate a Linux systemd user service; never authenticates or starts it"}
	install := &cobra.Command{Use: "install", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if runtime.GOOS != "linux" {
			return errors.New("systemd services require Linux; use aih start on Windows/macOS")
		}
		if os.Geteuid() == 0 {
			return errors.New("run service install as the non-root application user, not sudo/root")
		}
		dir, home, err := o.paths()
		if err != nil {
			return err
		}
		if _, err = config.ParseLocal(dir); err != nil {
			return fmt.Errorf("configure/clone the AIH application first: %w", err)
		}
		if err = config.OutsideSource(dir, home); err != nil {
			return err
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		envFile := o.envFile
		if envFile != "" {
			envFile, err = filepath.Abs(envFile)
			if err != nil {
				return err
			}
		}
		unit, err := service.Render(service.Options{Name: name, Binary: binary, Repository: dir, Home: home, EnvironmentFile: envFile, Path: os.Getenv("PATH")})
		if err != nil {
			return err
		}
		if printOnly {
			fmt.Fprint(cmd.OutOrStdout(), unit)
			return nil
		}
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			base = filepath.Join(userHome, ".config")
		}
		if !filepath.IsAbs(base) {
			return errors.New("XDG_CONFIG_HOME must be absolute")
		}
		unitDir := filepath.Join(base, "systemd", "user")
		if err = os.MkdirAll(unitDir, 0700); err != nil {
			return err
		}
		path := filepath.Join(unitDir, "aih-"+name+".service")
		if old, err := os.ReadFile(path); err == nil {
			if !bytes.Equal(old, []byte(unit)) {
				return fmt.Errorf("%s exists with different contents; stop the service and move that file to a backup before regenerating", path)
			}
		} else {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = f.WriteString(unit)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s (not started). PATH captured from this process; credentials were not copied.\nRun:\n  systemctl --user daemon-reload\n  systemctl --user enable --now aih-%s.service\nFor boot/SSH independence, an administrator must enable linger for this OS user; see docs/VM_SETUP.md.\n", path, name)
		return nil
	}}
	install.Flags().StringVar(&name, "name", "", "unique application service name (required)")
	_ = install.MarkFlagRequired("name")
	install.Flags().BoolVar(&printOnly, "print", false, "print the generated unit without writing it")
	parent.AddCommand(install)
	root.AddCommand(parent)
}
