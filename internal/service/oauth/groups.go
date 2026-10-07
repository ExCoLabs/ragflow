//
//  Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.
//

package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"ragflow/internal/common"
	"ragflow/internal/server/config"
	"regexp"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	defaultGraphURL = "https://graph.microsoft.com/v1.0"
	// checkMemberGroups accepts at most 20 group IDs per call.
	checkMemberGroupsBatch = 20
)

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// errUserNotInDirectory is returned by Graph for users unknown to the
// directory (external accounts); they simply belong to no group.
var errUserNotInDirectory = errors.New("user not found in directory")

// TeamPlan is the outcome of a group lookup: the teams (owner emails) the
// user must belong to and the teams the sync manages.
type TeamPlan struct {
	// Desired are the mapped teams of the user's groups plus the default teams.
	Desired []string
	// Managed are all mapping targets; memberships in managed teams outside
	// Desired are removed.
	Managed []string
	// Default teams are never removed.
	Default []string
}

// GroupSyncer resolves the teams of an SSO user for one channel.
type GroupSyncer struct {
	cfg   config.GroupSyncConfig
	graph *graphClient // nil when Graph credentials are unavailable

	mu       sync.Mutex
	groupIDs map[string]string // display name -> object ID, resolved once
}

var (
	syncersMu sync.Mutex
	syncers   = map[string]*GroupSyncer{}
)

// GetGroupSyncer returns the (cached) syncer of a channel, or nil when group
// sync is not enabled for it.
func GetGroupSyncer(channel string, cfg config.OAuthChannelConfig) *GroupSyncer {
	if !cfg.GroupSync.Enabled {
		return nil
	}
	key := fmt.Sprintf("%s|%#v", channel, cfg)
	syncersMu.Lock()
	defer syncersMu.Unlock()
	if s, ok := syncers[key]; ok {
		return s
	}
	s := &GroupSyncer{cfg: cfg.GroupSync, graph: newGraphClient(cfg), groupIDs: map[string]string{}}
	syncers[key] = s
	return s
}

// Applies reports whether the sync manages this email (domain filter).
func (s *GroupSyncer) Applies(email string) bool {
	if len(s.cfg.Domains) == 0 {
		return true
	}
	_, domain, _ := strings.Cut(email, "@")
	for _, d := range s.cfg.Domains {
		if strings.EqualFold(strings.TrimSpace(d), domain) {
			return true
		}
	}
	return false
}

// Plan looks up the user's groups and returns the team plan.
func (s *GroupSyncer) Plan(ctx context.Context, info *UserInfo) (*TeamPlan, error) {
	plan := &TeamPlan{Default: normalizeEmails(s.cfg.DefaultTeams)}
	managed := map[string]bool{}
	for _, teams := range s.cfg.Mapping {
		for _, t := range normalizeEmails(teams) {
			managed[t] = true
		}
	}
	plan.Managed = sortedKeys(managed)

	memberOf, err := s.memberGroups(ctx, info)
	if err != nil {
		return nil, err
	}
	desired := map[string]bool{}
	for _, t := range plan.Default {
		desired[t] = true
	}
	for key, teams := range s.cfg.Mapping {
		if memberOf[key] {
			for _, t := range normalizeEmails(teams) {
				desired[t] = true
			}
		}
	}
	plan.Desired = sortedKeys(desired)
	return plan, nil
}

// memberGroups returns the mapping keys whose group the user belongs to.
func (s *GroupSyncer) memberGroups(ctx context.Context, info *UserInfo) (map[string]bool, error) {
	if len(s.cfg.Mapping) == 0 {
		return map[string]bool{}, nil
	}
	if strings.EqualFold(s.cfg.Source, "claim") {
		if groups, ok := s.claimGroups(info); ok {
			return s.matchClaimGroups(ctx, groups), nil
		}
		common.Info("Groups claim missing or overage; falling back to Microsoft Graph", zap.String("email", info.Email))
	}
	if s.graph == nil {
		return nil, errors.New("group sync needs Microsoft Graph but no tenant / credentials are configured")
	}
	ids := s.resolveAll(ctx)
	byID := map[string]string{}
	var list []string
	for key, id := range ids {
		byID[strings.ToLower(id)] = key
		list = append(list, id)
	}
	sort.Strings(list)
	found, err := s.graph.checkMemberGroups(ctx, info.Email, list)
	if errors.Is(err, errUserNotInDirectory) {
		common.Info("SSO user not found in the directory; no groups", zap.String("email", info.Email))
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, id := range found {
		if key, ok := byID[strings.ToLower(id)]; ok {
			out[key] = true
		}
	}
	return out, nil
}

// claimGroups reads the groups claim. ok is false when the claim is absent,
// including Entra's group overage (`_claim_names` / `hasgroups`).
func (s *GroupSyncer) claimGroups(info *UserInfo) ([]string, bool) {
	name := s.cfg.Claim
	if name == "" {
		name = "groups"
	}
	if names, isMap := info.Claims["_claim_names"].(map[string]any); isMap {
		if _, overage := names[name]; overage {
			return nil, false
		}
	}
	if has, _ := info.Claims["hasgroups"].(bool); has {
		return nil, false
	}
	raw, present := info.Claims[name]
	if !present {
		return nil, false
	}
	var groups []string
	switch v := raw.(type) {
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok {
				groups = append(groups, s)
			}
		}
	case string:
		groups = splitFields(v)
	}
	return groups, true
}

func splitFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

// matchClaimGroups matches claim values against the mapping keys, both as
// written and as resolved object IDs (when Graph is available).
func (s *GroupSyncer) matchClaimGroups(ctx context.Context, groups []string) map[string]bool {
	have := map[string]bool{}
	for _, g := range groups {
		have[strings.ToLower(g)] = true
	}
	var ids map[string]string
	if s.graph != nil {
		ids = s.resolveAll(ctx)
	}
	out := map[string]bool{}
	for key := range s.cfg.Mapping {
		if have[strings.ToLower(key)] || (ids[key] != "" && have[strings.ToLower(ids[key])]) {
			out[key] = true
		}
	}
	return out
}

// resolveAll returns mapping key -> group object ID. Keys that are already
// GUIDs map to themselves; display names are looked up via Graph and cached.
// Unresolvable names are logged and skipped (and retried on the next login).
func (s *GroupSyncer) resolveAll(ctx context.Context) map[string]string {
	out := map[string]string{}
	for key := range s.cfg.Mapping {
		if guidPattern.MatchString(key) {
			out[key] = key
			continue
		}
		s.mu.Lock()
		id, ok := s.groupIDs[key]
		s.mu.Unlock()
		if !ok && s.graph != nil {
			var err error
			id, err = s.graph.groupIDByName(ctx, key)
			if err != nil {
				common.Warn("Cannot resolve group display name", zap.String("group", key), zap.Error(err))
				continue
			}
			s.mu.Lock()
			s.groupIDs[key] = id
			s.mu.Unlock()
		}
		if id != "" {
			out[key] = id
		}
	}
	return out
}

func normalizeEmails(in []string) []string {
	var out []string
	for _, e := range in {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// graphClient calls Microsoft Graph with an app-only token.
type graphClient struct {
	baseURL    string
	httpClient *http.Client
}

// newGraphClient returns nil when no directory tenant can be determined.
func newGraphClient(cfg config.OAuthChannelConfig) *graphClient {
	gs := cfg.GroupSync
	clientID, secret := gs.GraphClientID, gs.GraphClientSecret
	if clientID == "" {
		clientID, secret = cfg.ClientID, cfg.ClientSecret
	}
	tokenURL := gs.GraphTokenURL
	if tokenURL == "" {
		tenant := gs.GraphTenantID
		if tenant == "" {
			tenant = tenantFromIssuer(cfg.Issuer)
		}
		if tenant == "" {
			return nil
		}
		tokenURL = "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	}
	baseURL := strings.TrimRight(gs.GraphURL, "/")
	if baseURL == "" {
		baseURL = defaultGraphURL
	}
	scopeBase := baseURL
	if u, err := url.Parse(baseURL); err == nil {
		scopeBase = u.Scheme + "://" + u.Host
	}
	cc := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: secret,
		TokenURL:     tokenURL,
		Scopes:       []string{scopeBase + "/.default"},
		AuthStyle:    oauth2.AuthStyleInParams,
	}
	base := &http.Client{Timeout: httpTimeout}
	// The token source caches the app token until shortly before it expires.
	ts := cc.TokenSource(context.WithValue(context.Background(), oauth2.HTTPClient, base))
	return &graphClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout:   httpTimeout,
			Transport: &oauth2.Transport{Source: oauth2.ReuseTokenSource(nil, ts), Base: base.Transport},
		},
	}
}

// tenantFromIssuer extracts the tenant of an Entra ID issuer such as
// https://login.microsoftonline.com/<tenant>/v2.0.
func tenantFromIssuer(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || !strings.HasPrefix(u.Host, "login.microsoftonline.") {
		return ""
	}
	tenant, _, _ := strings.Cut(strings.Trim(u.Path, "/"), "/")
	return tenant
}

func (g *graphClient) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("graph %s %s: %s", method, path, resp.Status)
	}
	return resp.StatusCode, json.Unmarshal(data, out)
}

// checkMemberGroups returns the subset of groupIDs the user is a member of
// (transitively).
func (g *graphClient) checkMemberGroups(ctx context.Context, user string, groupIDs []string) ([]string, error) {
	var found []string
	for start := 0; start < len(groupIDs); start += checkMemberGroupsBatch {
		end := min(start+checkMemberGroupsBatch, len(groupIDs))
		var resp struct {
			Value []string `json:"value"`
		}
		status, err := g.do(ctx, http.MethodPost, "/users/"+url.PathEscape(user)+"/checkMemberGroups",
			map[string]any{"groupIds": groupIDs[start:end]}, &resp)
		if status == http.StatusNotFound {
			return nil, errUserNotInDirectory
		}
		if err != nil {
			return nil, err
		}
		found = append(found, resp.Value...)
	}
	return found, nil
}

// groupIDByName looks a group up by display name.
func (g *graphClient) groupIDByName(ctx context.Context, name string) (string, error) {
	q := url.Values{
		"$filter": {"displayName eq '" + strings.ReplaceAll(name, "'", "''") + "'"},
		"$select": {"id,displayName"},
		"$top":    {"2"},
	}
	var resp struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/groups?"+q.Encode(), nil, &resp); err != nil {
		return "", err
	}
	switch len(resp.Value) {
	case 0:
		return "", errors.New("no group with this display name")
	case 1:
	default:
		common.Warn("Several groups share a display name; using the first", zap.String("group", name))
	}
	return resp.Value[0].ID, nil
}
