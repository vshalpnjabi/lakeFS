package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/treeverse/lakefs/pkg/api/apigen"
	"github.com/treeverse/lakefs/pkg/uri"
)

var errBadTagResponse = errors.New("bad response from server")

const tagCreateRequiredArgs = 2

var tagCreateCmd = &cobra.Command{
	Use:     "create <tag URI> <commit URI>",
	Short:   "Create a new tag in a repository",
	Example: "lakectl tag create lakefs://example-repo/example-tag lakefs://example-repo/2397cc9a9d04c20a4e5739b42c1dd3d8ba655c0b3a3b974850895a13d8bf9917",
	Args:    cobra.ExactArgs(tagCreateRequiredArgs),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) >= tagCreateRequiredArgs {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return validRepositoryToComplete(cmd.Context(), toComplete)
	},
	Run: func(cmd *cobra.Command, args []string) {
		tagURI := MustParseRefURI("tag URI", args[0])
		commitURI := MustParseRefURI("commit URI", args[1])

		client := getClient()
		ctx := cmd.Context()
		force := Must(cmd.Flags().GetBool("force"))

		if tagURI.Repository != commitURI.Repository {
			Die("both references must belong to the same repository", 1)
		}

		ref, err := createTag(ctx, client, tagURI, commitURI, force)
		if err != nil {
			DieErr(err)
		}
		fmt.Printf("Created tag '%s' (%s)\n", tagURI.Ref, *ref)
	},
}

// createTag creates a tag. With --force it resolves the target commit first,
// remembers the old tag, then deletes and recreates. A failed recreate puts
// the old tag back and returns the original error.
func createTag(ctx context.Context, client apigen.ClientWithResponsesInterface, tagURI, commitURI *uri.URI, force bool) (*apigen.Ref, error) {
	commitRef := commitURI.Ref
	var previousCommitID string

	if force {
		// Resolve the target before deleting, so relative refs like tag~1 still work.
		res, err := client.GetCommitWithResponse(ctx, tagURI.Repository, commitURI.Ref)
		if err := RetrieveError(res, err); err != nil {
			return nil, err
		}
		if res.JSON200 == nil {
			return nil, errBadTagResponse
		}
		commitRef = res.JSON200.Id

		tagResp, err := client.GetTagWithResponse(ctx, tagURI.Repository, tagURI.Ref)
		if err != nil {
			return nil, err
		}
		if tagResp == nil {
			return nil, errBadTagResponse
		}
		if tagResp.JSON200 != nil {
			previousCommitID = tagResp.JSON200.CommitId
		} else if tagResp.JSON404 == nil {
			if e := RetrieveError(tagResp, nil); e != nil {
				return nil, e
			}
		}

		delResp, err := client.DeleteTagWithResponse(ctx, tagURI.Repository, tagURI.Ref, &apigen.DeleteTagParams{})
		if err != nil {
			return nil, err
		}
		if delResp == nil {
			return nil, errBadTagResponse
		}
		if delResp.JSON404 == nil {
			if e := RetrieveError(delResp, nil); e != nil {
				return nil, e
			}
		}
	}

	resp, err := client.CreateTagWithResponse(ctx, tagURI.Repository, apigen.CreateTagJSONRequestBody{
		Id:  tagURI.Ref,
		Ref: commitRef,
	})
	createErr := RetrieveError(resp, err)
	if createErr == nil && (resp == nil || resp.JSON201 == nil) {
		createErr = errBadTagResponse
	}
	if createErr != nil {
		if previousCommitID != "" {
			if restoreErr := restoreTag(ctx, client, tagURI.Repository, tagURI.Ref, previousCommitID); restoreErr != nil {
				return nil, fmt.Errorf("%w (also failed to restore previous tag: %s)", createErr, restoreErr)
			}
		}
		return nil, createErr
	}
	return resp.JSON201, nil
}

func restoreTag(ctx context.Context, client apigen.ClientWithResponsesInterface, repository, tagID, commitID string) error {
	resp, err := client.CreateTagWithResponse(ctx, repository, apigen.CreateTagJSONRequestBody{
		Id:  tagID,
		Ref: commitID,
	})
	if err := RetrieveError(resp, err); err != nil {
		return err
	}
	if resp == nil || resp.JSON201 == nil {
		return errBadTagResponse
	}
	return nil
}

//nolint:gochecknoinits
func init() {
	tagCreateCmd.Flags().BoolP("force", "f", false, "override the tag if it exists")
	tagCmd.AddCommand(tagCreateCmd)
}
