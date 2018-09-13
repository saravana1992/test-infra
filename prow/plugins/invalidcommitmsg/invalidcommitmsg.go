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

// Package invalidcommitmsg adds the "do-not-merge/invalid-commit-message"
// label on PRs containing commit messages with @mentions or
// keywords that can automatically close issues.
package invalidcommitmsg

import (
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

const (
	pluginName            = "invalidcommitmsg"
	invalidCommitMsgLabel = "do-not-merge/invalid-commit-message"
	commentBody           = `[Keywords](https://help.github.com/articles/closing-issues-using-keywords) which can automatically close issues and at(@) mentions are not allowed in commit messages.

Please remove these keywords from the following commit messages: `
)

var (
	closeIssueRegex = regexp.MustCompile(`(([cC]los(?:e[sd]?))|([fF]ix(?:(es|ed)?))|([rR]esolv(?:e[sd]?)))[\s:]+(\w+/\w+)?#(\d+)`)
	atMentionRegex  = regexp.MustCompile(`@[-\w]+`)
)

func init() {
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequest, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	// Only the Description field is specified because this plugin is not triggered with commands and is not configurable.
	return &pluginhelp.PluginHelp{
			Description: "The invalidcommitmsg plugin applies the '" + invalidCommitMsgLabel + "' label to pull requests whose commit messages contain @ mentions or keywords which can automatically close issues.",
		},
		nil
}

type githubClient interface {
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	CreateComment(owner, repo string, number int, comment string) error
	ListPullRequestCommits(org, repo string, number int) ([]github.RepositoryCommit, error)
}

type commentPruner interface {
	PruneComments(shouldPrune func(github.IssueComment) bool)
}

func handlePullRequest(pc plugins.PluginClient, pr github.PullRequestEvent) error {
	return handle(pc.GitHubClient, pc.Logger, pr, pc.CommentPruner)
}

func handle(gc githubClient, log *logrus.Entry, pr github.PullRequestEvent, cp commentPruner) error {
	// Only consider actions indicating that the code diffs may have changed.
	if !isPRChanged(pr) {
		return nil
	}

	var (
		org    = pr.Repo.Owner.Login
		repo   = pr.Repo.Name
		number = pr.Number
	)

	labels, err := gc.GetIssueLabels(org, repo, pr.Number)
	if err != nil {
		return err
	}
	hasInvalidCommitMsgLabel := github.HasLabel(invalidCommitMsgLabel, labels)

	repoCommits, err := gc.ListPullRequestCommits(org, repo, number)
	if err != nil {
		return err
	}

	// If a commit message involves an invalid keyword,
	// add the commit SHA to a slice of invalid commits.
	invalidCommitSHAs := []string{}
	for _, repoCommit := range repoCommits {
		if closeIssueRegex.MatchString(repoCommit.Commit.Message) || atMentionRegex.MatchString(repoCommit.Commit.Message) {
			invalidCommitSHAs = append(invalidCommitSHAs, repoCommit.SHA[0:7]) // show only the first 7 digits of the commit SHA
		}
	}

	// if we have the label but all commits are valid,
	// remove the label and prune comments
	if hasInvalidCommitMsgLabel && len(invalidCommitSHAs) == 0 {
		if err := gc.RemoveLabel(org, repo, number, invalidCommitMsgLabel); err != nil {
			log.WithError(err).Errorf("Github failed to remove the following label: %s", invalidCommitMsgLabel)
		}
		cp.PruneComments(func(comment github.IssueComment) bool {
			return strings.Contains(comment.Body, commentBody)
		})
		return nil
	}

	// if we don't have the label and there are invalid commits,
	// add the label
	if !hasInvalidCommitMsgLabel && len(invalidCommitSHAs) != 0 {
		if err := gc.AddLabel(org, repo, number, invalidCommitMsgLabel); err != nil {
			log.WithError(err).Errorf("Github failed to add the following label: %s", invalidCommitMsgLabel)
		}
	}

	// if there are invalid commits, always add a comment
	if len(invalidCommitSHAs) != 0 {
		resp := commentBody + strings.Join(invalidCommitSHAs, ", ")
		formattedComment := plugins.FormatSimpleResponse(pr.PullRequest.User.Login, resp)
		if err := gc.CreateComment(org, repo, pr.Number, formattedComment); err != nil {
			log.WithError(err).Errorf("Failed to comment %q", formattedComment)
		}
	}

	return nil
}

// these are the only actions indicating that the code diffs may have changed.
func isPRChanged(pr github.PullRequestEvent) bool {
	switch pr.Action {
	case github.PullRequestActionOpened:
		return true
	case github.PullRequestActionReopened:
		return true
	case github.PullRequestActionSynchronize:
		return true
	default:
		return false
	}
}
