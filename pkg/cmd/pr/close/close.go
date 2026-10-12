package close

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cli/cli/v2/api"
	ghContext "github.com/cli/cli/v2/context"
	"github.com/cli/cli/v2/git"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/pkg/cmd/pr/shared"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
)

type CloseOptions struct {
	HttpClient func() (*http.Client, error)
	GitClient  *git.Client
	IO         *iostreams.IOStreams
	Branch     func() (string, error)
	Remotes    func() (ghContext.Remotes, error)

	Finder shared.PRFinder

	SelectorArg       string
	Comment           string
	DeleteBranch      bool
	DeleteLocalBranch bool
}

func NewCmdClose(f *cmdutil.Factory, runF func(*CloseOptions) error) *cobra.Command {
	opts := &CloseOptions{
		IO:         f.IOStreams,
		HttpClient: f.HttpClient,
		GitClient:  f.GitClient,
		Branch:     f.Branch,
		Remotes:    f.Remotes,
	}

	cmd := &cobra.Command{
		Use:   "close {<number> | <url> | <branch>}",
		Short: "Close a pull request",
		Args:  cmdutil.ExactArgs(1, "cannot close pull request: number, url, or branch required"),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Finder = shared.NewFinder(f)

			if len(args) > 0 {
				opts.SelectorArg = args[0]
			}

			opts.DeleteLocalBranch = !cmd.Flags().Changed("repo")

			if runF != nil {
				return runF(opts)
			}
			return closeRun(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.Comment, "comment", "c", "", "Leave a closing comment")
	cmd.Flags().BoolVarP(&opts.DeleteBranch, "delete-branch", "d", false, "Delete the local and remote branch after close")

	return cmd
}

func closeRun(opts *CloseOptions) error {
	cs := opts.IO.ColorScheme()

	findOptions := shared.FindOptions{
		Selector: opts.SelectorArg,
		Fields:   []string{"state", "number", "title", "isCrossRepository", "headRefName", "headRefOid"},
	}
	pr, baseRepo, err := opts.Finder.Find(findOptions)
	if err != nil {
		return err
	}

	if pr.State == "MERGED" {
		fmt.Fprintf(opts.IO.ErrOut, "%s Pull request %s#%d (%s) can't be closed because it was already merged\n", cs.FailureIcon(), ghrepo.FullName(baseRepo), pr.Number, pr.Title)
		return cmdutil.SilentError
	} else if !pr.IsOpen() {
		fmt.Fprintf(opts.IO.ErrOut, "%s Pull request %s#%d (%s) is already closed\n", cs.WarningIcon(), ghrepo.FullName(baseRepo), pr.Number, pr.Title)
		return nil
	}

	httpClient, err := opts.HttpClient()
	if err != nil {
		return err
	}

	if opts.Comment != "" {
		commentOpts := &shared.CommentableOptions{
			Body:       opts.Comment,
			HttpClient: opts.HttpClient,
			InputType:  shared.InputTypeInline,
			Quiet:      true,
			RetrieveCommentable: func() (shared.Commentable, ghrepo.Interface, error) {
				return pr, baseRepo, nil
			},
		}
		err := shared.CommentableRun(commentOpts)
		if err != nil {
			return err
		}
	}

	err = api.PullRequestClose(httpClient, baseRepo, pr.ID)
	if err != nil {
		return fmt.Errorf("API call failed: %w", err)
	}

	fmt.Fprintf(opts.IO.ErrOut, "%s Closed pull request %s#%d (%s)\n", cs.SuccessIconWithColor(cs.Red), ghrepo.FullName(baseRepo), pr.Number, pr.Title)

	if opts.DeleteBranch {
		ctx := context.Background()
		branchSwitchString := ""
		apiClient := api.NewClientFromHTTP(httpClient)
		localBranchTip, tipErr := opts.GitClient.LocalBranchTip(ctx, pr.HeadRefName)

		if opts.DeleteLocalBranch {
			if tipErr == nil {
				if ok, reason := localBranchSafeToDelete(opts, pr, baseRepo, localBranchTip, ctx); !ok {
					fmt.Fprintf(opts.IO.ErrOut, "%s Skipping local branch delete: %s\n", cs.WarningIcon(), reason)
				} else {
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
						err = opts.GitClient.CheckoutBranch(ctx, branchToSwitchTo)
						if err != nil {
							return err
						}
					}

					if err := opts.GitClient.DeleteLocalBranch(ctx, pr.HeadRefName); err != nil {
						return fmt.Errorf("failed to delete local branch %s: %w", cs.Cyan(pr.HeadRefName), err)
					}

					if branchToSwitchTo != "" {
						branchSwitchString = fmt.Sprintf(" and switched to branch %s", cs.Cyan(branchToSwitchTo))
					}
				}
			} else {
				fmt.Fprintf(opts.IO.ErrOut, "%s Skipped deleting the local branch since current directory is not a git repository\n", cs.WarningIcon())
			}
		}

		if pr.IsCrossRepository {
			fmt.Fprintf(opts.IO.ErrOut, "%s Skipped deleting the remote branch of a pull request from fork\n", cs.WarningIcon())
			if !opts.DeleteLocalBranch {
				return nil
			}
		} else {
			if err := api.BranchDeleteRemote(apiClient, baseRepo, pr.HeadRefName); err != nil {
				return fmt.Errorf("failed to delete remote branch %s: %w", cs.Cyan(pr.HeadRefName), err)
			}
		}
		fmt.Fprintf(opts.IO.ErrOut, "%s Deleted branch %s%s\n", cs.SuccessIconWithColor(cs.Red), cs.Cyan(pr.HeadRefName), branchSwitchString)
	}

	return nil
}

// localBranchSafeToDelete reports whether the local branch matching the pull
// request's head branch may be deleted, and if not, why.
//
// A pull request can be selected by URL, which leaves local cleanup enabled
// but says nothing about which local repository gh is standing in. Because
// the head branch would otherwise be matched by name alone, a same-named
// branch in an unrelated repository used to be force-deleted along with the
// pull request's branch. Both checks fail closed: unless the branch is
// clearly the pull request's own, the local repository is left untouched.
func localBranchSafeToDelete(opts *CloseOptions, pr *api.PullRequest, baseRepo ghrepo.Interface, localBranchTip string, ctx context.Context) (bool, string) {
	// The pull request has to belong to a repository this directory is a clone
	// of, otherwise a same-named branch found here is somebody else's work.
	// Remotes are compared by owner and name rather than by URL string: the
	// same repository is reachable through many URL spellings, and matching on
	// one of them would mistake a clone of the right repository for a clone of
	// an unrelated one. Only the base repository counts: a fork whose name
	// collides with the base repository's would make the check a name
	// comparison again, which is the bug being fixed here.
	if opts.Remotes == nil {
		return false, "could not determine the repositories of the current directory"
	}
	remotes, err := opts.Remotes()
	if err != nil {
		return false, fmt.Sprintf("could not determine the repositories of the current directory: %s", err)
	}
	if _, err := remotes.FindByRepo(baseRepo.RepoOwner(), baseRepo.RepoName()); err != nil {
		return false, fmt.Sprintf("this directory is not a clone of %s", ghrepo.FullName(baseRepo))
	}

	// The branch can be in the right repository and still be work of its own:
	// deleting it runs `git branch -D`, which skips the merged-check git would
	// otherwise apply, so only delete when every commit on the local branch is
	// already part of the pull request head.
	if pr.HeadRefOid == "" {
		// headRefOid is populated by the pull request query above and is
		// non-null for a real pull request, so an empty value means it was not
		// populated rather than that there is nothing to compare against.
		// Keep the pre-guard behavior instead of guessing which way to err.
		return true, ""
	}
	onPRHead, err := opts.GitClient.IsAncestor(ctx, localBranchTip, pr.HeadRefOid)
	if err != nil {
		return false, fmt.Sprintf("could not compare local branch %s with the head of pull request %s#%d: %s", pr.HeadRefName, ghrepo.FullName(baseRepo), pr.Number, err)
	}
	if !onPRHead {
		return false, fmt.Sprintf("local branch %s has commits that are not in pull request %s#%d", pr.HeadRefName, ghrepo.FullName(baseRepo), pr.Number)
	}
	return true, ""
}
