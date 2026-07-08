package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/crypto"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// loadCodec builds the storage codec from the configured keyset. Every
// plumbing command shares it; a missing keyset must surface as an error so
// filter.agentbrain.required=true fails closed (spec §5).
func loadCodec() (*crypto.Codec, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return nil, err
	}
	primitive, err := keys.Primitive(paths.Keyset())
	if err != nil {
		return nil, fmt.Errorf("keyset unavailable (run `agent-brain init` or `agent-brain key import`): %w", err)
	}
	return crypto.NewCodec(primitive), nil
}

// The endpoint logic lives on crypto.Codec (Clean/Smudge — spec §8). git-clean
// always needs the codec now (Clean verify-decrypts magic input before it may
// pass through), so it has no keyset-less path; git-smudge/textconv keep their
// keyset-less passthrough for never-encrypted plaintext, so a clone without a
// keyset can still read never-filtered files.

func newGitCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-clean",
		Short:  "Filter: encrypt stdin to stdout (invoked by git)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// nil passthrough: git-clean has no keyset-less short-circuit —
			// Clean verify-decrypts magic input, so both branches (verify
			// existing ciphertext, encrypt plaintext) need the codec.
			return pipeFilter(cmd, func(codec *crypto.Codec, data []byte) ([]byte, error) {
				return codec.Clean(data)
			}, nil)
		},
	}
}

func newGitSmudgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-smudge",
		Short:  "Filter: decrypt stdin to stdout (invoked by git)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return pipeFilter(cmd, func(codec *crypto.Codec, data []byte) ([]byte, error) {
				return codec.Smudge(data)
			}, func(data []byte) bool { return !crypto.IsEncrypted(data) }) // plaintext passes through without a keyset
		},
	}
}

func newGitTextconvCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-textconv <file>",
		Short:  "Diff textconv: print decrypted file (invoked by git)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// args[0] is the blob path git supplies to its textconv filter, a
			// trusted invocation argument rather than untrusted user input.
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			if !crypto.IsEncrypted(data) {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			codec, err := loadCodec()
			if err != nil {
				return err
			}
			plaintext, err := codec.Smudge(data)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(plaintext)
			return err
		},
	}
}

// pipeFilter reads stdin and applies the codec endpoint. A nil passthrough
// predicate means the endpoint always needs the keyset (git-clean must
// verify-decrypt magic input, so it can never short-circuit keyset-less). A
// non-nil predicate short-circuits to raw passthrough when it reports true —
// git-smudge/textconv pass never-encrypted plaintext through without a keyset,
// so a clone lacking one can still read never-filtered files.
func pipeFilter(cmd *cobra.Command, endpoint func(*crypto.Codec, []byte) ([]byte, error), passthrough func([]byte) bool) error {
	input, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return err
	}
	if passthrough != nil && passthrough(input) {
		_, err = cmd.OutOrStdout().Write(input)
		return err
	}
	codec, err := loadCodec()
	if err != nil {
		return err
	}
	output, err := endpoint(codec, input)
	if err != nil {
		return err
	}
	_, err = cmd.OutOrStdout().Write(output)
	return err
}
