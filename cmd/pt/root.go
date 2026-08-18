package main

import (
	"github.com/spf13/cobra"
)

// globalFlags are the flags every subcommand honors.
type globalFlags struct {
	JSON    bool
	Verbose bool
}

var global globalFlags

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
	}

	root.PersistentFlags().BoolVar(&global.JSON, "json", false, "machine-readable output (list, ports)")
	root.PersistentFlags().BoolVarP(&global.Verbose, "verbose", "v", false, "verbose output")

	root.AddCommand(
		newInitCmd(),
		newAllowCmd(),
		newShellCmd(),
		newPortsCmd(),
		newListCmd(),
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
		RunE:  func(cmd *cobra.Command, args []string) error { panic("TODO(wave3): pt init") },
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
		RunE: func(cmd *cobra.Command, args []string) error { panic("TODO(wave3): pt allow") },
	}
}

func newShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell [path]",
		Short: "Enter the project's VM, creating it if needed",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { panic("TODO(wave3): pt shell") },
	}
}

func newPortsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ports",
		Short: "Show configured forwards and their live status",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { panic("TODO(wave3): pt ports") },
	}
	cmd.Flags().Bool("global", false, "show forwards for every project, flagging collisions")
	return cmd
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show active instances with resource usage",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { panic("TODO(wave3): pt list") },
	}
}

// Hidden subcommands. These are implementation details of pt itself; users
// invoking them directly is unsupported.

func newSuperviseCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_supervise",
		Short:  "Instance supervisor (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   func(cmd *cobra.Command, args []string) error { panic("TODO(wave3): pt _supervise") },
	}
}

func newCheckTrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_check-trust <dir>",
		Short:  "Fast trust check for the shell plugin (internal)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE:   func(cmd *cobra.Command, args []string) error { panic("TODO(wave3): pt _check-trust") },
	}
}

func newZSHHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "zsh-hook",
		Short:  "Print the zsh integration for `source <(pt zsh-hook)`",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   func(cmd *cobra.Command, args []string) error { panic("TODO(wave3): pt zsh-hook") },
	}
}
