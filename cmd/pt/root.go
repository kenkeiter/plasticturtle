package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kenkeiter/plasticturtle/internal/banner"
	"github.com/kenkeiter/plasticturtle/internal/shell"
	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/zshplugin"
)

// globalFlags are the flags every subcommand honors.
type globalFlags struct {
	JSON    bool
	Verbose bool
}

var global globalFlags

// argPath returns the optional [path] argument, or "" for the working
// directory.
func argPath(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "pt",
		Short: "Run project directories inside ephemeral Tart VMs",
		Long: "Plastic Turtle manages ephemeral Tart VM instances that sandbox project\n" +
			"directories. A project opts in with a .plasticturtle file; pt shell clones,\n" +
			"boots, and connects to a VM with the project mounted, and tears it down when\n" +
			"the last shell exits.",
		SilenceUsage:  true,
		SilenceErrors: false,
		Version:       version,
		// Bare `pt` is someone looking around rather than asking for anything,
		// so it gets the logo above the usage cobra would have printed on its
		// own. NoArgs keeps `pt bogus` an unknown-command error instead of
		// quietly showing the banner.
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			banner.Fprint(cmd.OutOrStdout())
			// Help writes to the same place and can only fail if that write
			// fails, which is not worth reporting on a torn-down stdout.
			_ = cmd.Help()
		},
	}

	root.PersistentFlags().BoolVar(&global.JSON, "json", false, "machine-readable output (list, ports)")
	root.PersistentFlags().BoolVarP(&global.Verbose, "verbose", "v", false, "verbose output")

	root.AddCommand(
		newInitCmd(),
		newAllowCmd(),
		newShellCmd(),
		newPortsCmd(),
		newListCmd(),
		newSetupFirewallCmd(),
		newSuperviseCmd(),
		newCheckTrustCmd(),
		newZSHHookCmd(),
	)
	return root
}

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init [path]",
		Short: "Set up a project interactively and write .plasticturtle",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv()
			if err != nil {
				return err
			}
			return runInit(e, argPath(args), cmd.OutOrStdout(), isTerminal(os.Stdin))
		},
	}
}

func newAllowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "allow [path]",
		Short: "Trust the current contents of .plasticturtle",
		Long: "Prints exactly what the config grants — image, resources, every mount and\n" +
			"mode, every port — and records approval of those exact bytes after you\n" +
			"confirm. This is the security choke point; a config must be re-allowed\n" +
			"whenever it changes.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv()
			if err != nil {
				return err
			}
			err = runAllow(e, argPath(args), cmd.InOrStdin(), cmd.OutOrStdout())
			if errors.Is(err, errDeclined) {
				// Declining is a choice, not a failure: exit non-zero so a
				// script can tell, but do not print an error at the user.
				exitStatus = 1
				return nil
			}
			return err
		},
	}
}

func newShellCmd() *cobra.Command {
	var persist bool
	cmd := &cobra.Command{
		Use:   "shell [path]",
		Short: "Enter the project's VM, creating it if needed",
		Long: "Clones the project's image, boots it with the project mounted, and logs in.\n" +
			"The clone is destroyed when the last shell exits, so nothing outside the\n" +
			"mounts survives — which is the point, and is what --persist opts out of.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv()
			if err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate the pt binary: %w", err)
			}

			var tty *os.File
			if isTerminal(os.Stdin) {
				tty = os.Stdin
			}

			opts := shell.Opts{
				Path:     argPath(args),
				Verbose:  global.Verbose,
				Persist:  persist,
				In:       os.Stdin,
				Out:      os.Stdout,
				Err:      os.Stderr,
				TTY:      tty,
				SelfPath: self,
			}
			deps := shell.Deps{
				Tart:  e.Tart,
				Store: e.Store,
				Trust: e.Trust,
				Clock: sys.RealClock(),
				Creds: sshx.DefaultCredentials(),
				Spawn: shell.RealSpawner(),
			}

			// shell.Run reports the remote shell's status, which is not an error
			// condition — a remote exit of 3 must become pt's exit of 3. The
			// error, when there is one, is left for cobra to print.
			code, err := shell.Run(cmd.Context(), opts, deps)
			exitStatus = code
			return err
		},
	}
	cmd.Flags().BoolVar(&persist, "persist", false,
		"boot the base image itself instead of a throwaway clone, keeping everything the guest writes")
	return cmd
}

func newPortsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ports [path]",
		Short: "Show configured forwards and their live status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv()
			if err != nil {
				return err
			}
			globalScope, err := cmd.Flags().GetBool("global")
			if err != nil {
				return err
			}
			return runPorts(e, argPath(args), cmd.OutOrStdout(), globalScope, global.JSON)
		},
	}
	cmd.Flags().Bool("global", false, "show forwards for every project, flagging collisions")
	return cmd
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show active instances with resource usage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv()
			if err != nil {
				return err
			}
			return runList(e, cmd.OutOrStdout(), global.JSON)
		},
	}
}

func newSetupFirewallCmd() *cobra.Command {
	var shim string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install the domain-firewall shim (one-time, needs sudo)",
		Long: "Installs pt's software-networking shim and makes it setuid-root so that\n" +
			"projects with a restricted `network:` policy can enforce their domain\n" +
			"allowlist. This is a one-time setup; re-run it after upgrading pt.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := openEnv()
			if err != nil {
				return err
			}
			return runSetupFirewall(e.Store, shim, cmd.OutOrStdout(), sudoRunner)
		},
	}
	cmd.Flags().StringVar(&shim, "shim", "", "path to the pt-softnet-shim binary (default: next to pt)")
	return cmd
}

// Hidden subcommands. These are implementation details of pt itself; users
// invoking them directly is unsupported.

func newSuperviseCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_supervise",
		Short:  "Instance supervisor (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSupervise(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// newCheckTrustCmd exists so that `pt help` and `pt _check-trust` with the
// wrong argument count behave sanely. The hot path does not come through here:
// main serves it before the command tree is built. See checkTrust.
func newCheckTrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_check-trust <dir>",
		Short:  "Fast trust check for the shell plugin (internal)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exitStatus = checkTrust(args[0])
			return nil
		},
	}
}

func newZSHHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "zsh-hook",
		Short:  "Print the zsh integration for `source <(pt zsh-hook)`",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprint(cmd.OutOrStdout(), zshplugin.Script())
			return err
		},
	}
}
