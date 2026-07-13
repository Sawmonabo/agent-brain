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
		// Args + RunE make the bare command the hub entry point (spec §1;
		// ADR 20 decision 1) instead of cobra's default help-and-exit-0.
		// NoArgs still leaves a mistyped subcommand failing with cobra's own
		// unknown-command error (NoArgs produces that exact message itself
		// when Find resolves no subcommand match back to root) — never
		// routed into runHub.
		Args: cobra.NoArgs,
		RunE: runHub,
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
		newInitCmd(),
		newTrackCmd(),
		newUntrackCmd(),
		newMigrateCmd(),
		newScanCmd(),
		newDashboardCmd(),
		newUpdateCmd(),
	)
	return root
}
