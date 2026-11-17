package main

import (
	"net/url"
	"os"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

func init() {
	cmd := &cobra.Command{
		Use: "syndra-branch <issue-id>", Short: "Resolve a Syndra bug's personal version branch or join its confirmation",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := cli.APIContext(cmd.Context())
			defer cancel()
			issue, err := resolveIssueRef(ctx, client, args[0])
			if err != nil {
				return err
			}
			repo, _ := cmd.Flags().GetString("repo")
			candidates, _ := cmd.Flags().GetStringArray("candidate")
			missing, _ := cmd.Flags().GetString("missing-decision")
			var result map[string]any
			err = client.PostJSON(ctx, "/api/issues/"+url.PathEscape(issue.ID)+"/syndra-branch", map[string]any{"repo": repo, "candidates": candidates, "missing_decision": missing}, &result)
			if err != nil {
				return err
			}
			return cli.PrintJSON(os.Stdout, result)
		},
	}
	cmd.Flags().String("repo", "", "Verified remote repository URL")
	cmd.Flags().StringArray("candidate", nil, "Remote version branch to propose (repeatable)")
	cmd.Flags().String("missing-decision", "", "Reopen a personal default whose confirmed branch no longer exists; pass its decision comment UUID")
	_ = cmd.MarkFlagRequired("repo")
	issueCmd.AddCommand(cmd)
}
