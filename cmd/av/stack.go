package main

import (
	"emperror.dev/errors"
	"fmt"
	"github.com/aviator-co/av/internal/git"
	"github.com/aviator-co/av/internal/stacks"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"strconv"
	"strings"
)

var stackCmd = &cobra.Command{
	Use:   "stack",
	Short: "managed stacked pull requests",
}

var stackBranchFlags struct {
	// The parent branch to base the new branch off.
	// By default, this is the current branch.
	Parent string
}
var stackBranchCmd = &cobra.Command{
	Use:   "branch <branch name>",
	Short: "create a new stacked branch",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			_ = cmd.Usage()
			return errors.New("exactly one branch name is required")
		}
		repo, err := getRepo()
		if err != nil {
			return err
		}
		err = stacks.CreateBranch(repo, &stacks.CreateBranchOpts{
			Name: args[0],
		})
		if err != nil {
			return err
		}
		fmt.Printf("Created branch %s\n", args[0])
		return nil
	},
}

var stackSyncFlags struct {
	// Set the parent of the current branch to this branch.
	// This effectively re-roots the stack on a new parent (e.g., adds a branch
	// to the stack).
	Parent string
	// If set, only sync up to the current branch (do not sync descendants).
	// This is useful for syncing changes from a parent branch in case the
	// current branch needs to be updated before continuing the sync.
	Current bool
	// If set, incorporate changes from the trunk (repo base branch) into the stack.
	// Only valid if synchronizing the root of a stack.
	// This effectively re-roots the stack on the latest commit from the trunk.
	Trunk bool
	// If set, do not push to GitHub.
	NoPush bool
	// If set, we're continuing a previous sync.
	// TODO:
	// 	 we might not actually need this, we can probably detect that
	//   a sync needs to be completed automagically and do the right thing.
	Continue bool
}
var stackSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "synchronize stacked branches",
	Long: strings.TrimSpace(`
Synchronize stacked branches to be up-to-date with their parent branches.

By default, this command will sync all branches starting at the root of the
stack and recursively rebasing each branch based on the latest commit from the
parent branch.

If the --current flag is given, this command will not recursively sync dependent
branches of the current branch within the stack. This allows you to make changes
to the current branch before syncing the rest of the stack.

If the --trunk flag is given, this command will synchronize changes from the
latest commit to the repository base branch (e.g., main or master) into the
stack. This is useful for rebasing a whole stack on the latest changes from the
base branch.
`),
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := getRepo()
		if err != nil {
			return err
		}

		diff, err := repo.Diff(&git.DiffOpts{Quiet: true})
		if err != nil {
			return err
		}
		if !diff.Empty {
			return errors.New("refusing to sync: there are unstaged changes in the working tree")
		}

		originalBranch, err := repo.CurrentBranchName()
		if err != nil {
			return err
		}

		defer func() {
			if _, err := repo.CheckoutBranch(&git.CheckoutBranch{Name: originalBranch}); err != nil {
				logrus.WithError(err).Warnf("failed to reset to original branch: %q", originalBranch)
			}
		}()

		root, err := stacks.GetCurrentRoot(repo)
		if err != nil {
			return err
		}
		printStackTree(root, 0)

		if len(root.Next) == 0 {
			// this shouldn't happen, but just in case
			return errors.New("no branches to sync")
		}

		current := root.Next[0]
		for {
			if _, err := repo.CheckoutBranch(&git.CheckoutBranch{
				Name: current.Branch.Name,
			}); err != nil {
				return errors.WrapIff(err, "failed to checkout branch %q", current.Branch.Name)
			}
			res, err := stacks.SyncBranch(repo, &stacks.SyncBranchOpts{
				Parent: current.Branch.Parent,
			})
			if err != nil {
				return errors.WrapIff(err, "failed to sync branch %q", current.Branch.Name)
			}
			switch res.Status {
			case stacks.SyncAlreadyUpToDate:
				fmt.Printf("Branch %q is already up-to-date with %q\n", current.Branch.Name, current.Branch.Parent)
			case stacks.SyncUpdated:
				fmt.Printf("Branch %q synchronized with %q\n", current.Branch.Name, current.Branch.Parent)
			case stacks.SyncConflict:
				fmt.Printf("Branch %q has merge conflict with %q, aborting...\n", current.Branch.Name, current.Branch.Parent)
				return nil
			default:
				logrus.Panicf("invariant error: unknown sync result: %v", res)
			}

			if len(current.Next) == 0 {
				return nil
			}
			if len(current.Next) > 1 {
				return errors.Errorf("unsupported: branch %q has more than one child branch", current.Branch.Name)
			}
			current = current.Next[0]
		}
	},
}

var stackTreeCmd = &cobra.Command{
	Use:   "tree",
	Short: "show the tree of stacked branches",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := getRepo()
		if err != nil {
			return err
		}
		trees, err := stacks.GetTrees(repo)
		if err != nil {
			return err
		}
		for _, tree := range trees {
			printStackTree(tree, 0)
		}
		return nil
	},
}

func printStackTree(tree *stacks.Tree, depth int) {
	indent := strings.Repeat("    ", depth)
	_, _ = fmt.Printf("%s%s\n", indent, tree.Branch.Name)
	for _, next := range tree.Next {
		printStackTree(next, depth+1)
	}
}

var stackNextFlags struct {
	// If set, synchronize changes from the parent branch after checking out
	// the next branch.
	Sync bool
}
var stackNextCmd = &cobra.Command{
	Use:   "next <n>",
	Short: "checkout the next branch in the stack",
	Long: strings.TrimSpace(`
Checkout the next branch in the stack.

If the --sync flag is given, this command will also synchronize changes from the
parent branch (i.e., the current branch before this command is run) into the
child branch (without recursively syncing further descendants).
`),
	RunE: func(cmd *cobra.Command, args []string) error {
		var n int = 1
		if len(args) == 1 {
			var err error
			n, err = strconv.Atoi(args[0])
			if err != nil {
				return errors.New("invalid number")
			}
		} else if len(args) > 1 {
			_ = cmd.Usage()
			return errors.New("too many arguments")
		}

		if n <= 0 {
			return errors.New("invalid number (must be >= 1)")
		}

		return errors.New("unimplemented")
	},
}

var stackPrevCmd = &cobra.Command{
	Use:   "prev <n>",
	Short: "checkout the previous branch in the stack",
	RunE: func(cmd *cobra.Command, args []string) error {
		var n int = 1
		if len(args) == 1 {
			var err error
			n, err = strconv.Atoi(args[0])
			if err != nil {
				return errors.Wrap(err, "invalid number")
			}
		} else if len(args) > 1 {
			_ = cmd.Usage()
			return errors.New("too many arguments")
		}

		if n <= 0 {
			return errors.New("invalid number (must be >= 1)")
		}

		return errors.New("unimplemented")
	},
}

func init() {
	stackCmd.AddCommand(
		stackBranchCmd,
		stackSyncCmd,
		stackTreeCmd,
		stackNextCmd,
		stackPrevCmd,
	)

	// av stack branch
	stackBranchCmd.Flags().StringVar(
		&stackBranchFlags.Parent, "parent", "",
		"parent branch to stack on",
	)

	// av stack sync
	stackSyncCmd.Flags().StringVar(
		&stackSyncFlags.Parent, "parent", "",
		"set the stack parent to this branch",
	)
	stackSyncCmd.Flags().BoolVar(
		&stackSyncFlags.Current, "current", false,
		"only sync changes to the current branch\n(don't recurse into descendant branches)",
	)
	stackSyncCmd.Flags().BoolVar(
		&stackSyncFlags.NoPush, "no-push", false,
		"do not force-push updated branches to GitHub",
	)
	stackSyncCmd.Flags().BoolVar(
		&stackSyncFlags.Trunk, "trunk", false,
		"synchronize the trunk into the stack",
	)
	stackSyncCmd.Flags().BoolVar(
		&stackSyncFlags.Continue, "continue", false,
		"continue a previous sync",
	)

	// av stack next
	stackNextCmd.Flags().BoolVar(
		&stackNextFlags.Sync, "sync", false,
		"synchronize changes from the parent branch",
	)
}