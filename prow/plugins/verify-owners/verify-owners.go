/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package verifyowners

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/config/org"
	"k8s.io/test-infra/prow/git"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/labels"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/plugins/golint"
	"k8s.io/test-infra/prow/repoowners"
	"sigs.k8s.io/yaml"
)

const (
	// PluginName defines this plugin's registered name.
	PluginName            = "verify-owners"
	ownersFileName        = "OWNERS"
	ownersAliasesFileName = "OWNERS_ALIASES"
)

func init() {
	plugins.RegisterPullRequestHandler(PluginName, handlePullRequest, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	return &pluginhelp.PluginHelp{
			Description: fmt.Sprintf("The verify-owners plugin validates %s and %s files if they are modified in a PR. On validation failure it automatically adds the '%s' label to the PR, and a review comment on the incriminating file(s).", ownersFileName, ownersAliasesFileName, labels.InvalidOwners),
		},
		nil
}

type ownersClient interface {
	LoadRepoOwners(org, repo, base string) (repoowners.RepoOwner, error)
}

type githubClient interface {
	AddLabel(org, repo string, number int, label string) error
	CreateComment(owner, repo string, number int, comment string) error
	CreateReview(org, repo string, number int, r github.DraftReview) error
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	RemoveLabel(owner, repo string, number int, label string) error
}

func handlePullRequest(pc plugins.Agent, pre github.PullRequestEvent) error {
	if pre.Action != github.PullRequestActionOpened && pre.Action != github.PullRequestActionReopened && pre.Action != github.PullRequestActionSynchronize {
		return nil
	}
	return handle(pc.GitHubClient, pc.GitClient, pc.Logger, &pre, pc.PluginConfig.Owners.LabelsBlackList)
}

type messageWithLine struct {
	line    int
	message string
}

func handle(ghc githubClient, gc *git.Client, log *logrus.Entry, pre *github.PullRequestEvent, labelsBlackList []string) error {
	org := pre.Repo.Owner.Login
	repo := pre.Repo.Name
	wrongOwnersFiles := map[string]messageWithLine{}
	ownersAliasesFileError := messageWithLine{}
	var wrongOwnersAliasesFilePresent bool
	var members sets.String
	var nonMembers sets.String

	// Get changes.
	changes, err := ghc.GetPullRequestChanges(org, repo, pre.Number)
	if err != nil {
		return fmt.Errorf("error getting PR changes: %v", err)
	}

	// Check if the OWNERS_ALIASES file was modified
	var modifiedOwnerAliasesFile github.PullRequestChange
	var ownerAliasesModified bool
	for _, change := range changes {
		if change.Filename == ownersAliasesFileName {
			modifiedOwnerAliasesFile = change
			ownerAliasesModified = true
		}
	}

	// List modified OWNERS files.
	var modifiedOwnersFiles []github.PullRequestChange
	for _, change := range changes {
		if filepath.Base(change.Filename) == ownersFileName {
			modifiedOwnersFiles = append(modifiedOwnersFiles, change)
		}
	}

	if len(modifiedOwnersFiles) == 0 && !ownerAliasesModified {
		return nil
	}

	// Clone the repo, checkout the PR.
	r, err := gc.Clone(pre.Repo.FullName)
	if err != nil {
		return err
	}
	defer func() {
		if err := r.Clean(); err != nil {
			log.WithError(err).Error("Error cleaning up repo.")
		}
	}()
	if err := r.CheckoutPullRequest(pre.Number); err != nil {
		return err
	}

	// Check the OWNERS_ALIASES file
	var repoAliases repoowners.RepoAliases
	if ownerAliasesModified {
		path := filepath.Join(r.Dir, ownersAliasesFileName)
		b, err := ioutil.ReadFile(path)
		if err != nil {
			log.WithError(err).Errorf("Failed to read %s.", path)
			return nil
		}
		// by default we bind errors to line 1
		lineNumber := 1
		repoAliases, err = repoowners.ParseAliasesConfig(b)
		if err != nil {
			lineNumberRe, _ := regexp.Compile(`line (\d+)`)
			lineNumberMatches := lineNumberRe.FindStringSubmatch(err.Error())
			// try to find a line number for the error
			if len(lineNumberMatches) > 1 {
				// we're sure it will convert as it passed the regexp already
				absoluteLineNumber, _ := strconv.Atoi(lineNumberMatches[1])
				// we need to convert it to a line number relative to the patch
				al, err := golint.AddedLines(modifiedOwnerAliasesFile.Patch)
				if err != nil {
					log.WithError(err).Errorf("Failed to compute added lines in %s: %v", ownersAliasesFileName, err)
				} else if val, ok := al[absoluteLineNumber]; ok {
					lineNumber = val
				}
			}
			ownersAliasesFileError = messageWithLine{
				lineNumber,
				fmt.Sprintf("Cannot parse file: %v.", err),
			}
			wrongOwnersAliasesFilePresent = true
		}

		if members.Len() == 0 {
			members, err = getMembersForOrg(org)
			if err != nil {
				return fmt.Errorf("failed to get members for org %s: %v", org, err)
			}
		}
		users := repoAliases.ExpandAllAliases()
		nonMembers = users.Difference(members)
	}

	// If OWNERS file changed but OWNERS_ALIASES didn't,
	// get the OWNERS_ALIASES file to check if the users
	// listed in the OWNERS file are org members or aliases.
	if len(modifiedOwnersFiles) != 0 && !ownerAliasesModified {
		path := filepath.Join(r.Dir, ownersAliasesFileName)
		b, err := ioutil.ReadFile(path)
		if err != nil {
			log.WithError(err).Errorf("Failed to read %s.", path)
			return nil
		}
		repoAliases, err = repoowners.ParseAliasesConfig(b)
		return err
	}

	// Check each OWNERS file.
	for _, c := range modifiedOwnersFiles {
		// Try to load OWNERS file.
		path := filepath.Join(r.Dir, c.Filename)
		b, err := ioutil.ReadFile(path)
		if err != nil {
			log.WithError(err).Errorf("Failed to read %s.", path)
			return nil
		}
		var approvers, reviewers, requiredReviewers, labels []string
		// by default we bind errors to line 1
		lineNumber := 1
		simple, err := repoowners.ParseSimpleConfig(b)
		if err != nil || simple.Empty() {
			full, err := repoowners.ParseFullConfig(b)
			if err != nil {
				lineNumberRe, _ := regexp.Compile(`line (\d+)`)
				lineNumberMatches := lineNumberRe.FindStringSubmatch(err.Error())
				// try to find a line number for the error
				if len(lineNumberMatches) > 1 {
					// we're sure it will convert as it passed the regexp already
					absoluteLineNumber, _ := strconv.Atoi(lineNumberMatches[1])
					// we need to convert it to a line number relative to the patch
					al, err := golint.AddedLines(c.Patch)
					if err != nil {
						log.WithError(err).Errorf("Failed to compute added lines in %s: %v", c.Filename, err)
					} else if val, ok := al[absoluteLineNumber]; ok {
						lineNumber = val
					}
				}
				wrongOwnersFiles[c.Filename] = messageWithLine{
					lineNumber,
					fmt.Sprintf("Cannot parse file: %v.", err),
				}
				continue
			} else {
				// it's a FullConfig
				for _, config := range full.Filters {
					approvers = append(approvers, config.Approvers...)
					labels = append(labels, config.Labels...)
					reviewers = append(reviewers, config.Reviewers...)
					requiredReviewers = append(requiredReviewers, config.RequiredReviewers...)
				}
			}
		} else {
			// it's a SimpleConfig
			approvers = simple.Config.Approvers
			labels = simple.Config.Labels
			reviewers = simple.Config.Reviewers
			requiredReviewers = simple.Config.RequiredReviewers
		}
		// Check labels against blacklist
		if sets.NewString(labels...).HasAny(labelsBlackList...) {
			wrongOwnersFiles[c.Filename] = messageWithLine{
				lineNumber,
				fmt.Sprintf("File contains blacklisted labels: %s.", sets.NewString(labels...).Intersection(sets.NewString(labelsBlackList...)).List()),
			}
			continue
		}
		// Check approvers isn't empty
		if filepath.Dir(c.Filename) == "." && len(approvers) == 0 {
			wrongOwnersFiles[c.Filename] = messageWithLine{
				lineNumber,
				fmt.Sprintf("No approvers defined in this root directory %s file.", ownersFileName),
			}
			continue
		}
		// Check if all listed users are members
		if members.Len() == 0 {
			members, err = getMembersForOrg(org)
			if err != nil {
				return fmt.Errorf("failed to get members for org %s: %v", org, err)
			}
		}
		nonMembers = nonMembers.Union(getNonMembersFromLists(repoAliases, members, approvers, reviewers, requiredReviewers))
	}

	// React if we saw something.
	if len(wrongOwnersFiles) > 0 || wrongOwnersAliasesFilePresent || len(nonMembers) > 0 {
		s := "s"
		if len(wrongOwnersFiles) == 1 {
			s = ""
		}
		if err := ghc.AddLabel(org, repo, pre.Number, labels.InvalidOwners); err != nil {
			return err
		}

		var comments []github.DraftReviewComment
		if len(wrongOwnersFiles) > 0 {
			log.Debugf("Creating a review for %d %s file%s.", len(wrongOwnersFiles), ownersFileName, s)
			for errFile, err := range wrongOwnersFiles {
				comments = append(comments, github.DraftReviewComment{
					Path:     errFile,
					Body:     err.message,
					Position: err.line,
				})
			}
		}

		if wrongOwnersAliasesFilePresent {
			log.Debugf("Creating a review for the %s file.", ownersAliasesFileName)
			comments = append(comments, github.DraftReviewComment{
				Path:     ownersAliasesFileName,
				Body:     ownersAliasesFileError.message,
				Position: ownersAliasesFileError.line,
			})
		}

		// Make the review body.
		var response, nonMemberResp string
		response = fmt.Sprintf("Adding the %s label because of the following errors:\n", labels.InvalidOwners)
		if len(wrongOwnersFiles) > 0 {
			response = response + fmt.Sprintf("- %d invalid %s file%s", len(wrongOwnersFiles), ownersFileName, s)
		}
		if wrongOwnersAliasesFilePresent {
			response = response + fmt.Sprintf("- An invalid %s file", ownersAliasesFileName)
		}
		if len(nonMembers) > 0 {
			listofNonMembers := nonMembers.List()
			nonMemberResp = fmt.Sprintf("- The following users are not members of the %s GitHub org. Membership is mandatory for being listed in an %s file. Instructions for applying for membership can be found [here](https://git.k8s.io/community/community-membership.md#member).", org, ownersFileName)
			for _, user := range listofNonMembers {
				nonMemberResp = nonMemberResp + "\n" + fmt.Sprintf("  - @%s", user)
			}
			response = response + nonMemberResp
		}

		draftReview := github.DraftReview{
			Body:     plugins.FormatResponseRaw(pre.PullRequest.Body, pre.PullRequest.HTMLURL, pre.PullRequest.User.Login, response),
			Action:   github.Comment,
			Comments: comments,
		}
		if pre.PullRequest.Head.SHA != "" {
			draftReview.CommitSHA = pre.PullRequest.Head.SHA
		}
		err := ghc.CreateReview(org, repo, pre.Number, draftReview)
		if err != nil {
			return fmt.Errorf("error creating a review for %s: %v", response, err)
		}
	} else {
		// Don't bother checking if it has the label...it's a race, and we'll have
		// to handle failure due to not being labeled anyway.
		if err := ghc.RemoveLabel(org, repo, pre.Number, labels.InvalidOwners); err != nil {
			return fmt.Errorf("failed removing %s label: %v", labels.InvalidOwners, err)
		}
	}
	return nil
}

func getMembersForOrg(orgName string) (sets.String, error) {
	var members sets.String
	url := fmt.Sprintf("https://raw.githubusercontent.com/kubernetes/org/master/config/%s/org.yaml", orgName)
	resp, err := http.Get(url)
	if err != nil {
		return members, err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return members, fmt.Errorf("unable to read the content at %s: %v", url, err)
	}

	config := org.Config{}
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal org config: %v", err)
	}

	return sets.NewString(config.Members...), nil
}

func getNonMembersFromLists(repoAliases repoowners.RepoAliases, members sets.String, lists ...[]string) sets.String {
	var totalUsers sets.String
	for _, list := range lists {
		totalUsers = totalUsers.Union(sets.NewString(list...))
		for _, login := range list {
			// if it is an alias, remove it
			if _, ok := repoAliases[login]; ok {
				totalUsers.Delete(login)
			}
		}
	}
	return totalUsers.Difference(members)
}
