/*
Copyright 2016 The Kubernetes Authors.

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

package plugins

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/github/fakegithub"
	"sigs.k8s.io/yaml"
)

func TestHasSelfApproval(t *testing.T) {
	cases := []struct {
		name     string
		cfg      string
		expected bool
	}{
		{
			name:     "self approval by default",
			expected: true,
		},
		{
			name:     "has approval when implicit_self_approve true",
			cfg:      `{"implicit_self_approve": true}`,
			expected: true,
		},
		{
			name: "reject approval when implicit_self_approve false",
			cfg:  `{"implicit_self_approve": false}`,
		},
		{
			name:     "reject approval when require_self_approval set",
			cfg:      `{"require_self_approval": true}`,
			expected: false,
		},
		{
			name:     "has approval when require_self_approval set to false",
			cfg:      `{"require_self_approval": false}`,
			expected: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Approve
			if err := yaml.Unmarshal([]byte(tc.cfg), &a); err != nil {
				t.Fatalf("failed to unmarshal cfg: %v", err)
			}
			if actual := a.HasSelfApproval(); actual != tc.expected {
				t.Errorf("%t != expected %t", actual, tc.expected)
			}
		})
	}
}

func TestConsiderReviewState(t *testing.T) {
	cases := []struct {
		name     string
		cfg      string
		expected bool
	}{
		{
			name:     "consider by default",
			expected: true,
		},
		{
			name:     "consider when draaa = true",
			cfg:      `{"review_acts_as_approve": true}`,
			expected: true,
		},
		{
			name: "do not consider when draaa = false",
			cfg:  `{"review_acts_as_approve": false}`,
		},
		{
			name: "do not consider when irs = true",
			cfg:  `{"ignore_review_state": true}`,
		},
		{
			name:     "consider when irs = false",
			cfg:      `{"ignore_review_state": false}`,
			expected: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var a Approve
			if err := yaml.Unmarshal([]byte(tc.cfg), &a); err != nil {
				t.Fatalf("failed to unmarshal cfg: %v", err)
			}
			if actual := a.ConsiderReviewState(); actual != tc.expected {
				t.Errorf("%t != expected %t", actual, tc.expected)
			}
		})
	}
}

func TestGetPlugins(t *testing.T) {
	var testcases = []struct {
		name            string
		pluginMap       map[string][]string // this is read from the plugins.yaml file typically.
		owner           string
		repo            string
		expectedPlugins []string
	}{
		{
			name: "All plugins enabled for org should be returned for any org/repo query",
			pluginMap: map[string][]string{
				"org1": {"plugin1", "plugin2"},
			},
			owner:           "org1",
			repo:            "repo",
			expectedPlugins: []string{"plugin1", "plugin2"},
		},
		{
			name: "All plugins enabled for org/repo should be returned for a org/repo query",
			pluginMap: map[string][]string{
				"org1":      {"plugin1", "plugin2"},
				"org1/repo": {"plugin3"},
			},
			owner:           "org1",
			repo:            "repo",
			expectedPlugins: []string{"plugin1", "plugin2", "plugin3"},
		},
		{
			name: "Plugins for org1/repo should not be returned for org2/repo query",
			pluginMap: map[string][]string{
				"org1":      {"plugin1", "plugin2"},
				"org1/repo": {"plugin3"},
			},
			owner:           "org2",
			repo:            "repo",
			expectedPlugins: nil,
		},
		{
			name: "Plugins for org1 should not be returned for org2/repo query",
			pluginMap: map[string][]string{
				"org1":      {"plugin1", "plugin2"},
				"org2/repo": {"plugin3"},
			},
			owner:           "org2",
			repo:            "repo",
			expectedPlugins: []string{"plugin3"},
		},
	}
	for _, tc := range testcases {
		pa := ConfigAgent{configuration: &Configuration{Plugins: tc.pluginMap}}

		plugins := pa.getPlugins(tc.owner, tc.repo)
		if len(plugins) != len(tc.expectedPlugins) {
			t.Errorf("Different number of plugins for case \"%s\". Got %v, expected %v", tc.name, plugins, tc.expectedPlugins)
		} else {
			for i := range plugins {
				if plugins[i] != tc.expectedPlugins[i] {
					t.Errorf("Different plugin for case \"%s\": Got %v expected %v", tc.name, plugins, tc.expectedPlugins)
				}
			}
		}
	}
}

func TestPullRequest(t *testing.T) {
	testCases := []struct {
		name   string
		agent  *Agent
		verify func(*Agent, github.PullRequest, error) error
	}{
		{
			name:  "Error when event is not a pull request",
			agent: &Agent{},
			verify: func(_ *Agent, _ github.PullRequest, err error) error {
				if err == nil || err.Error() != "event was not for a pull request" {
					return fmt.Errorf("Expected error to be 'event was not for a pull request', was %q", err)
				}
				return nil
			},
		},
		{
			name:  "Existing PullRequest is returned",
			agent: &Agent{isPR: true, pullRequest: &github.PullRequest{ID: 123456}},
			verify: func(a *Agent, pr github.PullRequest, err error) error {
				if err != nil {
					return fmt.Errorf("expected err to be nil, was %v", err)
				}
				if a.pullRequest == nil || a.pullRequest.ID != 123456 {
					return fmt.Errorf("Expected agent to contain pr with id 123456, pr was %v", a.pullRequest)
				}
				if pr.ID != 123456 {
					return fmt.Errorf("Expected returned pr.ID to be 123456, was %d", pr.ID)
				}
				return nil
			},
		},
		{
			name: "PullRequest is fetched, stored and returned",
			agent: &Agent{
				lock: &sync.Mutex{},
				isPR: true,
				GitHubClient: &fakegithub.FakeClient{
					PullRequests: map[int]*github.PullRequest{0: &github.PullRequest{ID: 123456}}},
			},
			verify: func(a *Agent, pr github.PullRequest, err error) error {
				if err != nil {
					return fmt.Errorf("expected err to be nil, was %v", err)
				}
				if a.pullRequest == nil || a.pullRequest.ID != 123456 {
					return fmt.Errorf("expected agent to contain pr with id 123456, pr was %v", a.pullRequest)
				}
				if pr.ID != 123456 {
					return fmt.Errorf("expected returned pr.ID to be 123456, was %d", pr.ID)
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pr, err := tc.agent.PullRequest()
			if err := tc.verify(tc.agent, pr, err); err != nil {
				t.Fatalf("verification failed: %v", err)
			}
		})
	}
}

func TestBaseSHA(t *testing.T) {
	testCases := []struct {
		name   string
		agent  *Agent
		verify func(*Agent, string, error) error
	}{
		{
			name:  "No pr, error",
			agent: &Agent{},
			verify: func(_ *Agent, _ string, err error) error {
				if err == nil || err.Error() != "event was not for a pull request" {
					return fmt.Errorf("Expected error to be 'event was not for a pull request', was %q", err)
				}
				return nil
			},
		},
		{
			name:  "Existing baseSHA is returned",
			agent: &Agent{isPR: true, baseSHA: "12345"},
			verify: func(a *Agent, baseSHA string, err error) error {
				if err != nil {
					return fmt.Errorf("expected err to be nil, was %v", err)
				}
				if a.baseSHA != "12345" {
					return fmt.Errorf("expected agent baseSHA to be 12345, was %q", a.baseSHA)
				}
				if baseSHA != "12345" {
					return fmt.Errorf("expected returned baseSHA to be 12345, was %q", baseSHA)
				}
				return nil
			},
		},
		{
			name: "BaseSHA is fetched, stored and returned",
			agent: &Agent{
				isPR:         true,
				lock:         &sync.Mutex{},
				GitHubClient: &fakegithub.FakeClient{},
				pullRequest:  &github.PullRequest{},
			},
			verify: func(a *Agent, baseSHA string, err error) error {
				if err != nil {
					return fmt.Errorf("expected err to be nil, was %v", err)
				}
				if a.baseSHA != fakegithub.TestRef {
					return fmt.Errorf("expected baseSHA on agent to be %q, was %q", fakegithub.TestRef, a.baseSHA)
				}
				if baseSHA != fakegithub.TestRef {
					return fmt.Errorf("expected returned baseSHA to be %q, was %q", fakegithub.TestRef, baseSHA)
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			baseSHA, err := tc.agent.BaseSHA()
			if err := tc.verify(tc.agent, baseSHA, err); err != nil {
				t.Fatalf("verification failed: %v", err)
			}
		})
	}
}

// TODO @alvaroaleman: Add more tests once InRepoConfig can be enabled
func TestPresubmits(t *testing.T) {
	testCases := []struct {
		name   string
		agent  *Agent
		verify func(*Agent, []config.Presubmit, error) error
	}{
		{
			name:  "Error when event is not a pull request",
			agent: &Agent{},
			verify: func(_ *Agent, _ []config.Presubmit, err error) error {
				if err == nil || err.Error() != "event was not for a pull request" {
					return fmt.Errorf("expected error to be 'event was not for a pull request', was %q", err)
				}
				return nil
			},
		},
		{
			name: "Inrepoconfig disabled, static presubmits are returned",
			agent: &Agent{
				isPR: true,
				org:  "my-org",
				repo: "my-repo",
				Config: &config.Config{
					JobConfig: config.JobConfig{
						Presubmits: map[string][]config.Presubmit{
							"my-org/my-repo": []config.Presubmit{{}, {}},
						},
					},
				},
			},
			verify: func(a *Agent, ps []config.Presubmit, err error) error {
				if err != nil {
					return fmt.Errorf("expected err to be nil, was %v", err)
				}
				if !reflect.DeepEqual(a.Config.JobConfig.Presubmits["my-org/my-repo"], ps) {
					return errors.New("returned presubmits are not identical to the ones in the config")
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := tc.agent.Presubmits()
			if err := tc.verify(tc.agent, ps, err); err != nil {
				t.Fatalf("verification failed: %v", err)
			}
		})
	}
}
