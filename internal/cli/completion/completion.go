package completion

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func NewCompletionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate autocompletion script",
		Long: `To load completions:

Bash:
  $ source <(cappctl completion bash)
  # To load for every session (Linux):
  $ cappctl completion bash > /etc/bash_completion.d/cappctl
  # To load for every session (macOS):
  $ cappctl completion bash > $(brew --prefix)/etc/bash_completion.d/cappctl

Zsh:
  $ source <(cappctl completion zsh)
  # To load for every session:
  $ cappctl completion zsh > "${fpath[1]}/_cappctl"
  # If not already in ~/.zshrc:
  # fpath=(~/.zsh/completions $fpath)
  # autoload -U compinit && compinit

Fish:
  $ cappctl completion fish | source
  # To load for every session:
  $ cappctl completion fish > ~/.config/fish/completions/cappctl.fish

PowerShell:
  PS> cappctl completion powershell | Out-String | Invoke-Expression
  # To load for every session, add the output to your PowerShell profile.
`,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		Run: func(cmd *cobra.Command, args []string) {
			var err error
			switch args[0] {
			case "bash":
				err = cmd.Root().GenBashCompletion(os.Stdout)
			case "zsh":
				err = cmd.Root().GenZshCompletion(os.Stdout)
			case "fish":
				err = cmd.Root().GenFishCompletion(os.Stdout, true)
			case "powershell":
				err = cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "error generating completion: %v\n", err)
				os.Exit(1)
			}
		},
	}
}
