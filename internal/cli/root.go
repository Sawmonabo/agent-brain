// Package cli assembles the agent-brain command tree (spec §7).
package cli

import "github.com/spf13/cobra"

// Version is stamped by the release build (-ldflags "-X ...cli.Version=v1.2.3").
var Version = "dev"

// Root returns the fully wired command tree. Later packages add subcommands
// here — main never changes.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "agent-brain",
		Short:         "Invisible cross-machine sync for AI coding agents' memory",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newGitCleanCmd(),
		newGitSmudgeCmd(),
		newGitTextconvCmd(),
		newGitMergeCmd(),
		newDaemonCmd(),
		newServiceCmd(),
		newStatusCmd(),
		newSyncCmd(),
		newProjectsCmd(),
		newDoctorCmd(),
		newKeyCmd(),
		newConflictsCmd(),
	)
	return root
}
