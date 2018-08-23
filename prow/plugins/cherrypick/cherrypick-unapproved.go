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

// Package cherrypick adds the `do-not-merge/cherry-pick-not-approved`
// label to PRs against a release branch which do not have the
// `cherrypick-approved` label.
package cherrypick

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

const pluginName = "cherrypick"

const (
	cpUnapprovedLabel         = "do-not-merge/cherry-pick-not-approved"
	cpApprovedLabel           = "cherrypick-approved"
	labelUnapprovedFormat     = "This PR is not for the master branch but does not have the `%s` label. Adding the `%s` label.\n"
	assignPatchReleaseManager = `
	To approve the cherrypick, please assign the patch release manager for the ` + "`%s`" + ` branch by writing ` + "`/assign @username`" + ` in a comment when ready.
The list of patch release managers for each release can be found [here](https://git.k8s.io/sig-release/release-managers.md).`
)

var (
	labelUnapprovedBody = fmt.Sprintf(labelUnapprovedFormat, cpApprovedLabel, cpUnapprovedLabel)
)

func init() {
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequest, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	/// Only the Description field is specified because this plugin is not triggered with commands and is not configurable.
	pluginHelp := &pluginhelp.PluginHelp{
		Description: "Label `do-not-merge` to PRs against a release branch which do not have `cherrypick-approved`",
	}
	return pluginHelp, nil
}

type githubClient interface {
	CreateComment(owner, repo string, number int, comment string) error
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
}

func handlePullRequest(pc plugins.PluginClient, pr github.PullRequestEvent) error {
	return handlePR(pc.GitHubClient, pc.Logger, &pr)
}

func handlePR(gc githubClient, log *logrus.Entry, pr *github.PullRequestEvent) error {
	// Only consider the events that indicate opening of the PR
	if pr.Action != github.PullRequestActionOpened && pr.Action != github.PullRequestActionReopened {
		return nil
	}

	var (
		org    = pr.Repo.Owner.Login
		repo   = pr.Repo.Name
		branch = pr.PullRequest.Base.Ref
	)

	// if it is not against a release branch, don't do anything
	if !strings.HasPrefix(branch, "release-") {
		return nil
	}

	labels, err := gc.GetIssueLabels(org, repo, pr.Number)
	if err != nil {
		log.WithError(err).Errorf("failed to list labels on  %s/%s#%d", org, repo, pr.Number)
	}
	hasCherryPickApprovedLabel := github.HasLabel(cpApprovedLabel, labels)
	hasCherryPickUnapprovedLabel := github.HasLabel(cpUnapprovedLabel, labels)

	// if it has the approved label,
	// remove the unapproved label (if it exists) and don't do anything
	if hasCherryPickApprovedLabel {
		if hasCherryPickUnapprovedLabel {
			if err := gc.RemoveLabel(org, repo, pr.Number, cpUnapprovedLabel); err != nil {
				log.WithError(err).Errorf("Github failed to remove the following label on  %s/%s#%d: %s", org, repo, pr.Number, cpUnapprovedLabel)
			}
		}
		return nil
	}

	// if it already has the unapproved label, we are done here
	if hasCherryPickUnapprovedLabel {
		return nil
	}

	// only add the label and comment if none of the approved and unapproved labels are present
	if err := gc.AddLabel(org, repo, pr.Number, cpUnapprovedLabel); err != nil {
		log.WithError(err).Errorf("Github failed to add the following label on %s/%s#%d: %s", org, repo, pr.Number, cpUnapprovedLabel)
	}

	body := labelUnapprovedBody + fmt.Sprintf(assignPatchReleaseManager, branch)
	comment := plugins.FormatSimpleResponse(pr.PullRequest.User.Login, body)
	if err := gc.CreateComment(org, repo, pr.Number, comment); err != nil {
		log.WithError(err).Errorf("Failed to comment on %s/%s#%d with comment %q.", org, repo, pr.Number, comment)
	}

	return nil
}
