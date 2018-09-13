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

package invalidcommitmsg

import (
	"reflect"
	"testing"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
)

type fakeClient struct {
	// current labels
	labels []string
	// labels that are added
	added []string
	// labels that are removed
	removed []string
	// commentsAdded tracks the comments in each PR
	commentsAdded map[int][]string
	// commitMessages tracks the commit messages in each PR
	commitMessages map[int][]string
}

// AddLabel adds a label to the specified PR or issue
func (fc *fakeClient) AddLabel(owner, repo string, number int, label string) error {
	fc.added = append(fc.added, label)
	fc.labels = append(fc.labels, label)
	return nil
}

// RemoveLabel removes the label from the specified PR or issue
func (fc *fakeClient) RemoveLabel(owner, repo string, number int, label string) error {
	fc.removed = append(fc.removed, label)

	// remove from existing labels
	for k, v := range fc.labels {
		if label == v {
			fc.labels = append(fc.labels[:k], fc.labels[k+1:]...)
			break
		}
	}

	return nil
}

// GetIssueLabels gets the current labels on the specified PR or issue
func (fc *fakeClient) GetIssueLabels(owner, repo string, number int) ([]github.Label, error) {
	la := []github.Label{}
	for _, l := range fc.labels {
		la = append(la, github.Label{Name: l})
	}
	return la, nil
}

// CreateComment adds and tracks a comment in the client
func (fc *fakeClient) CreateComment(owner, repo string, number int, comment string) error {
	fc.commentsAdded[number] = append(fc.commentsAdded[number], comment)
	return nil
}

// NumComments counts the number of tracked comments
func (fc *fakeClient) NumComments() int {
	n := 0
	for _, comments := range fc.commentsAdded {
		n += len(comments)
	}
	return n
}

// ListPullRequestCommits lists the commits in the PR
func (fc *fakeClient) ListPullRequestCommits(org, repo string, number int) ([]github.RepositoryCommit, error) {
	commits := []github.RepositoryCommit{}
	for _, msg := range fc.commitMessages[number] {
		commit := github.RepositoryCommit{
			SHA: "1111111111",
			Commit: github.Commit{
				Message: msg,
			},
		}
		commits = append(commits, commit)
	}

	return commits, nil
}

type fakePruner struct{}

func (fp *fakePruner) PruneComments(shouldPrune func(github.IssueComment) bool) {}

func makeFakePullRequestEvent(action github.PullRequestEventAction) github.PullRequestEvent {
	return github.PullRequestEvent{
		Action: action,
		Number: 5,
		PullRequest: github.PullRequest{
			Base: github.PullRequestBranch{
				Repo: github.Repo{
					Owner: github.User{
						Login: "kubernetes",
					},
					Name: "test-infra",
				},
			},
		},
	}
}

func TestInvalidCommitMessage(t *testing.T) {
	var testcases = []struct {
		name           string
		action         github.PullRequestEventAction
		commitMessages []string
		labels         []string
		added          []string
		removed        []string
		expectComment  bool
	}{
		{
			name:           "unsupported PR action -> no-op",
			action:         github.PullRequestActionEdited,
			commitMessages: []string{},
			labels:         []string{},
			added:          []string{},
			removed:        []string{},
			expectComment:  false,
		},
		{
			name:           "contains valid message -> no-op",
			action:         github.PullRequestActionReopened,
			commitMessages: []string{"this is a valid message", "fixing k/k#9999", "not a @ mention"},
			labels:         []string{},
			added:          []string{},
			removed:        []string{},
			expectComment:  false,
		},
		{
			name:           "msg contains @mention -> add label and comment",
			action:         github.PullRequestActionOpened,
			commitMessages: []string{"this is a @mention"},
			labels:         []string{},
			added:          []string{invalidCommitMsgLabel},
			removed:        []string{},
			expectComment:  true,
		},
		{
			name:           "msg contains the keyword fixes -> add label and comment",
			action:         github.PullRequestActionOpened,
			commitMessages: []string{"fixes #9999"},
			labels:         []string{},
			added:          []string{invalidCommitMsgLabel},
			removed:        []string{},
			expectComment:  true,
		},
		{
			name:           "msg contains the keyword close -> add label and comment",
			action:         github.PullRequestActionOpened,
			commitMessages: []string{"close k/k#9999"},
			labels:         []string{},
			added:          []string{invalidCommitMsgLabel},
			removed:        []string{},
			expectComment:  true,
		},
		{
			name:           "msg contains the keyword resolved -> add label and comment",
			action:         github.PullRequestActionOpened,
			commitMessages: []string{"resolved k/k#9999"},
			labels:         []string{},
			added:          []string{invalidCommitMsgLabel},
			removed:        []string{},
			expectComment:  true,
		},
		{
			name:           "msg contains the keyword fix and @mention -> add label and comment",
			action:         github.PullRequestActionOpened,
			commitMessages: []string{"fix #9999", "this is a @mention"},
			labels:         []string{},
			added:          []string{invalidCommitMsgLabel},
			removed:        []string{},
			expectComment:  true,
		},
		{
			name:           "msg contains invalid keywords but has label -> add comment",
			action:         github.PullRequestActionOpened,
			commitMessages: []string{"this @menti-on has a hyphen"},
			labels:         []string{invalidCommitMsgLabel},
			added:          []string{},
			removed:        []string{},
			expectComment:  true,
		},
		{
			name:           "msg does not contain invalid keywords but has label -> remove label",
			action:         github.PullRequestActionOpened,
			commitMessages: []string{"this is a valid message"},
			labels:         []string{invalidCommitMsgLabel},
			added:          []string{},
			removed:        []string{invalidCommitMsgLabel},
			expectComment:  false,
		},
		{
			name:           "msg does not contain invalid keywords but has label -> remove label",
			action:         github.PullRequestActionOpened,
			commitMessages: []string{"this is a valid message"},
			labels:         []string{invalidCommitMsgLabel},
			added:          []string{},
			removed:        []string{invalidCommitMsgLabel},
			expectComment:  false,
		},
	}

	for _, tc := range testcases {
		fc := &fakeClient{
			labels:         tc.labels,
			added:          []string{},
			removed:        []string{},
			commentsAdded:  make(map[int][]string, 0),
			commitMessages: make(map[int][]string, 0),
		}

		if len(tc.commitMessages) != 0 {
			fc.commitMessages[5] = tc.commitMessages
		}

		event := makeFakePullRequestEvent(tc.action)
		err := handle(fc, logrus.WithField("plugin", "fake-invalidcommitmsg"), event, &fakePruner{})
		switch {
		case err != nil:
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		case !reflect.DeepEqual(tc.added, fc.added):
			t.Errorf("%s: added %v != actual %v", tc.name, tc.added, fc.added)
		case !reflect.DeepEqual(tc.removed, fc.removed):
			t.Errorf("%s: removed %v != actual %v", tc.name, tc.removed, fc.removed)
		}

		// if we expected a comment, verify that a comment was made
		numComments := fc.NumComments()
		if tc.expectComment && numComments != 1 {
			t.Errorf("%s: expected 1 comment but received %d comments", tc.name, numComments)
		}
		if !tc.expectComment && numComments != 0 {
			t.Errorf("%s: expected no comments but received %d comments", tc.name, numComments)
		}
	}
}
