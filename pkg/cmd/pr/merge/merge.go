package merge

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/AlecAivazis/survey/v2"
	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/api"
	"github.com/cli/cli/context"
	"github.com/cli/cli/git"
	"github.com/cli/cli/internal/config"
	"github.com/cli/cli/internal/ghrepo"
	"github.com/cli/cli/pkg/cmd/pr/shared"
	"github.com/cli/cli/pkg/cmdutil"
	"github.com/cli/cli/pkg/iostreams"
	"github.com/cli/cli/pkg/prompt"
	"github.com/spf13/cobra"
)

type MergeOptions struct {
	HttpClient func() (*http.Client, error)
	Config     func() (config.Config, error)
	IO         *iostreams.IOStreams
	BaseRepo   func() (ghrepo.Interface, error)
	Remotes    func() (context.Remotes, error)
	Branch     func() (string, error)

	SelectorArg  string
	DeleteBranch bool
	MergeMethod  api.PullRequestMergeMethod

	IsDeleteBranchIndicated bool
	CanDeleteLocalBranch    bool
	InteractiveMode         bool
}

func NewCmdMerge(f *cmdutil.Factory, runF func(*MergeOptions) error) *cobra.Command {
	opts := &MergeOptions{
		IO:         f.IOStreams,
		HttpClient: f.HttpClient,
		Config:     f.Config,
		Remotes:    f.Remotes,
		Branch:     f.Branch,
	}

	var (
		flagMerge  bool
		flagSquash bool
		flagRebase bool
	)

	cmd := &cobra.Command{
		Use:   "merge [<number> | <url> | <branch>]",
		Short: "Merge a pull request",
		Long: heredoc.Doc(`
			Merge a pull request on GitHub.
    	`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// support `-R, --repo` override
			opts.BaseRepo = f.BaseRepo

			if repoOverride, _ := cmd.Flags().GetString("repo"); repoOverride != "" && len(args) == 0 {
				return &cmdutil.FlagError{Err: errors.New("argument required when using the --repo flag")}
			}

			if len(args) > 0 {
				opts.SelectorArg = args[0]
			}

			methodFlags := 0
			if flagMerge {
				opts.MergeMethod = api.PullRequestMergeMethodMerge
				methodFlags++
			}
			if flagRebase {
				opts.MergeMethod = api.PullRequestMergeMethodRebase
				methodFlags++
			}
			if flagSquash {
				opts.MergeMethod = api.PullRequestMergeMethodSquash
				methodFlags++
			}
			if methodFlags == 0 {
				if !opts.IO.CanPrompt() {
					return &cmdutil.FlagError{Err: errors.New("--merge, --rebase, or --squash required when not running interactively")}
				}
				opts.InteractiveMode = true
			} else if methodFlags > 1 {
				return &cmdutil.FlagError{Err: errors.New("only one of --merge, --rebase, or --squash can be enabled")}
			}

			opts.IsDeleteBranchIndicated = cmd.Flags().Changed("delete-branch")
			opts.CanDeleteLocalBranch = !cmd.Flags().Changed("repo")

			if runF != nil {
				return runF(opts)
			}
			return mergeRun(opts)
		},
	}

	cmd.Flags().BoolVarP(&opts.DeleteBranch, "delete-branch", "d", false, "Delete the local and remote branch after merge")
	cmd.Flags().BoolVarP(&flagMerge, "merge", "m", false, "Merge the commits with the base branch")
	cmd.Flags().BoolVarP(&flagRebase, "rebase", "r", false, "Rebase the commits onto the base branch")
	cmd.Flags().BoolVarP(&flagSquash, "squash", "s", false, "Squash the commits into one commit and merge it into the base branch")
	return cmd
}

func mergeRun(opts *MergeOptions) error {
	cs := opts.IO.ColorScheme()

	httpClient, err := opts.HttpClient()
	if err != nil {
		return err
	}
	apiClient := api.NewClientFromHTTP(httpClient)

	pr, baseRepo, err := shared.PRFromArgs(apiClient, opts.BaseRepo, opts.Branch, opts.Remotes, opts.SelectorArg)
	if err != nil {
		return err
	}

	if pr.Mergeable == "CONFLICTING" {
		fmt.Fprintf(opts.IO.ErrOut, "%s Pull request #%d (%s) has conflicts and isn't mergeable\n", cs.Red("!"), pr.Number, pr.Title)
		return cmdutil.SilentError
	}

	deleteBranch := opts.DeleteBranch
	crossRepoPR := pr.HeadRepositoryOwner.Login != baseRepo.RepoOwner()
	isTerminal := opts.IO.IsStdoutTTY()

	isPRAlreadyMerged := pr.State == "MERGED"
	if !isPRAlreadyMerged {
		mergeMethod := opts.MergeMethod

		if opts.InteractiveMode {
			mergeMethod, deleteBranch, err = prInteractiveMerge(opts, crossRepoPR)
			if err != nil {
				if errors.Is(err, cancelError) {
					fmt.Fprintln(opts.IO.ErrOut, "Cancelled.")
					return cmdutil.SilentError
				}
				return err
			}
		}

		err = api.PullRequestMerge(apiClient, baseRepo, pr, mergeMethod)
		if err != nil {
			return err
		}

		if isTerminal {
			action := "Merged"
			switch mergeMethod {
			case api.PullRequestMergeMethodRebase:
				action = "Rebased and merged"
			case api.PullRequestMergeMethodSquash:
				action = "Squashed and merged"
			}
			fmt.Fprintf(opts.IO.ErrOut, "%s %s pull request #%d (%s)\n", cs.Magenta("✔"), action, pr.Number, pr.Title)
		}
	} else if !opts.IsDeleteBranchIndicated && opts.InteractiveMode && !crossRepoPR {
		err := prompt.SurveyAskOne(&survey.Confirm{
			Message: fmt.Sprintf("Pull request #%d was already merged. Delete the branch locally?", pr.Number),
			Default: false,
		}, &deleteBranch)
		if err != nil {
			return fmt.Errorf("could not prompt: %w", err)
		}
	} else if crossRepoPR {
		fmt.Fprintf(opts.IO.ErrOut, "%s Pull request #%d was already merged\n", cs.WarningIcon(), pr.Number)
	}

	if !deleteBranch || crossRepoPR {
		return nil
	}

	branchSwitchString := ""

	if opts.CanDeleteLocalBranch {
		currentBranch, err := opts.Branch()
		if err != nil {
			return err
		}

		var branchToSwitchTo string
		if currentBranch == pr.HeadRefName {
			branchToSwitchTo, err = api.RepoDefaultBranch(apiClient, baseRepo)
			if err != nil {
				return err
			}
			err = git.CheckoutBranch(branchToSwitchTo)
			if err != nil {
				return err
			}
		}

		localBranchExists := git.HasLocalBranch(pr.HeadRefName)
		if localBranchExists {
			err = git.DeleteLocalBranch(pr.HeadRefName)
			if err != nil {
				err = fmt.Errorf("failed to delete local branch %s: %w", cs.Cyan(pr.HeadRefName), err)
				return err
			}
		}

		if branchToSwitchTo != "" {
			branchSwitchString = fmt.Sprintf(" and switched to branch %s", cs.Cyan(branchToSwitchTo))
		}
	}

	if !isPRAlreadyMerged {
		err = api.BranchDeleteRemote(apiClient, baseRepo, pr.HeadRefName)
		var httpErr api.HTTPError
		// The ref might have already been deleted by GitHub
		if err != nil && (!errors.As(err, &httpErr) || httpErr.StatusCode != 422) {
			err = fmt.Errorf("failed to delete remote branch %s: %w", cs.Cyan(pr.HeadRefName), err)
			return err
		}
	}

	if isTerminal {
		fmt.Fprintf(opts.IO.ErrOut, "%s Deleted branch %s%s\n", cs.Red("✔"), cs.Cyan(pr.HeadRefName), branchSwitchString)
	}

	return nil
}

var cancelError = errors.New("cancelError")

func prInteractiveMerge(opts *MergeOptions, crossRepoPR bool) (api.PullRequestMergeMethod, bool, error) {
	mergeMethodQuestion := &survey.Question{
		Name: "mergeMethod",
		Prompt: &survey.Select{
			Message: "What merge method would you like to use?",
			Options: []string{"Create a merge commit", "Rebase and merge", "Squash and merge"},
			Default: "Create a merge commit",
		},
	}

	qs := []*survey.Question{mergeMethodQuestion}

	if !crossRepoPR && !opts.IsDeleteBranchIndicated {
		var message string
		if opts.CanDeleteLocalBranch {
			message = "Delete the branch locally and on GitHub?"
		} else {
			message = "Delete the branch on GitHub?"
		}

		deleteBranchQuestion := &survey.Question{
			Name: "deleteBranch",
			Prompt: &survey.Confirm{
				Message: message,
				Default: false,
			},
		}
		qs = append(qs, deleteBranchQuestion)
	}

	qs = append(qs, &survey.Question{
		Name: "isConfirmed",
		Prompt: &survey.Confirm{
			Message: "Submit?",
			Default: false,
		},
	})

	answers := struct {
		MergeMethod  int
		DeleteBranch bool
		IsConfirmed  bool
	}{
		DeleteBranch: opts.DeleteBranch,
	}

	err := prompt.SurveyAsk(qs, &answers)
	if err != nil {
		return 0, false, fmt.Errorf("could not prompt: %w", err)
	}
	if !answers.IsConfirmed {
		return 0, false, cancelError
	}

	var mergeMethod api.PullRequestMergeMethod
	switch answers.MergeMethod {
	case 0:
		mergeMethod = api.PullRequestMergeMethodMerge
	case 1:
		mergeMethod = api.PullRequestMergeMethodRebase
	case 2:
		mergeMethod = api.PullRequestMergeMethodSquash
	}

	return mergeMethod, answers.DeleteBranch, nil
}
