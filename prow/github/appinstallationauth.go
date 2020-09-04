/*
Copyright 2020 The Kubernetes Authors.

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

package github

import (
	"fmt"
	"time"

	"strconv"
)

// TODO: This should be in its own package and ideally just expose a http.Transport for the github api client and a token generator for the git client

type installationTokenRequest struct {
	org, repo      string
	token          string
	installationID string
	err            error
	done           chan (struct{})
}

func newInstallationIDCache(c *client) *installationIDCache {
	cache := &installationIDCache{
		client:   c,
		tokens:   map[int]*AppToken{},
		requests: make(chan *installationTokenRequest),
	}
	go cache.start()
	return cache
}

// installationID cache mints an installation token for the given org/repo if possible.
// This involved getting the installationID and caching it, then getting a token, also
// caching it.
// In order to avoid a complicated locking system to make this threadsafe, we serialize
// all requests by sending them over a channel that has a single worker on the other
// side.
type installationIDCache struct {
	client           *client
	installationData []AppInstallation
	tokens           map[int]*AppToken
	requests         chan (*installationTokenRequest)
}

func (c *installationIDCache) get(org, repo string) (token string, installationID string, err error) {
	installationTokenRequest := &installationTokenRequest{
		org:  org,
		repo: repo,
		done: make(chan struct{}),
	}
	c.requests <- installationTokenRequest
	<-installationTokenRequest.done
	return installationTokenRequest.token, installationTokenRequest.installationID, installationTokenRequest.err
}

func (c *installationIDCache) start() {
	for request := range c.requests {
		request.token, request.installationID, request.err = c.work(request.org, request.repo)
		close(request.done)
	}
}

// TODO: Support repo installations
func (c *installationIDCache) work(org, repo string) (token string, installationID string, err error) {
	id, found := c.forOrgRepo(org, repo)
	if !found {
		if err := c.refreshInstallationData(); err != nil {
			return "", "", fmt.Errorf("failed to update app installations: %w", err)
		}
	}
	id, found = c.forOrgRepo(org, repo)
	if !found {
		return "", "", fmt.Errorf("no installation found for org %s", org)
	}

	if token, found := c.tokens[id]; found && token.ExpiresAt.Add(-time.Minute).After(time.Now()) {
		return token.Token, strconv.Itoa(id), nil
	}

	appToken, err := c.client.getAppInstallationToken(id)
	if err != nil {
		return "", "", fmt.Errorf("failed to get token for installation with id %d: %w", id, err)
	}
	c.tokens[id] = appToken
	return appToken.Token, strconv.Itoa(id), nil
}

func (c *installationIDCache) forOrgRepo(org, repo string) (int, bool) {
	for _, installation := range c.installationData {
		if installation.Account.Login == org {
			return installation.ID, true
		}
	}

	return 0, false
}

func (c *installationIDCache) refreshInstallationData() error {
	result, err := c.client.getAppInstallations()

	if err != nil {
		return err
	}
	c.installationData = result
	return nil
}
