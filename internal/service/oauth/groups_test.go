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

package oauth_test

import (
	"context"
	"slices"
	"testing"

	"ragflow/internal/server/config"
	"ragflow/internal/service/oauth"
	"ragflow/internal/service/oauth/oauthtest"
)

const (
	groupEngineering = "11111111-1111-1111-1111-111111111111"
	groupSales       = "22222222-2222-2222-2222-222222222222"
)

func groupSyncChannel(name string, g *oauthtest.Graph, gs config.GroupSyncConfig) (string, config.OAuthChannelConfig) {
	gs.Enabled = true
	gs.GraphURL, gs.GraphTokenURL = g.URL(), g.TokenURL()
	return name, config.OAuthChannelConfig{
		Type:         "oidc",
		Issuer:       "https://login.microsoftonline.com/tenant-1/v2.0",
		ClientID:     oauthtest.ClientID,
		ClientSecret: oauthtest.ClientSecret,
		GroupSync:    gs,
	}
}

func TestGroupSyncPlanFromGraph(t *testing.T) {
	g := oauthtest.NewGraph(t)
	g.Groups["Sales Team"] = groupSales
	g.Members["alice@example.com"] = []string{groupEngineering, groupSales}
	g.Members["bob@example.com"] = []string{groupSales}

	syncer := oauth.GetGroupSyncer(groupSyncChannel(t.Name(), g, config.GroupSyncConfig{
		Mapping: map[string][]string{
			groupEngineering: {"eng-team@example.com"},
			"Sales Team":     {"sales-team@example.com", "Shared-Team@example.com"},
		},
		DefaultTeams: []string{"everyone@example.com"},
		Domains:      []string{"example.com"},
	}))
	if !syncer.Applies("Alice@Example.com") || syncer.Applies("admin@other.org") {
		t.Fatal("domain filter is wrong")
	}

	plan, err := syncer.Plan(context.Background(), &oauth.UserInfo{Email: "bob@example.com"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if want := []string{"everyone@example.com", "sales-team@example.com", "shared-team@example.com"}; !slices.Equal(plan.Desired, want) {
		t.Errorf("desired = %v, want %v", plan.Desired, want)
	}
	if want := []string{"eng-team@example.com", "sales-team@example.com", "shared-team@example.com"}; !slices.Equal(plan.Managed, want) {
		t.Errorf("managed = %v, want %v", plan.Managed, want)
	}

	plan, err = syncer.Plan(context.Background(), &oauth.UserInfo{Email: "alice@example.com"})
	if err != nil || !slices.Contains(plan.Desired, "eng-team@example.com") {
		t.Fatalf("alice plan = %+v, %v", plan, err)
	}
	if n := g.CallCount("lookup"); n != 1 {
		t.Errorf("display name looked up %d times, want once (cached)", n)
	}
	if n := g.CallCount("token"); n != 1 {
		t.Errorf("app token fetched %d times, want once (cached)", n)
	}

	// Users unknown to the directory belong to no group.
	plan, err = syncer.Plan(context.Background(), &oauth.UserInfo{Email: "guest@example.com"})
	if err != nil || !slices.Equal(plan.Desired, []string{"everyone@example.com"}) {
		t.Fatalf("guest plan = %+v, %v", plan, err)
	}
}

func TestGroupSyncPlanFromClaim(t *testing.T) {
	g := oauthtest.NewGraph(t)
	g.Members["carol@example.com"] = []string{groupSales}
	syncer := oauth.GetGroupSyncer(groupSyncChannel(t.Name(), g, config.GroupSyncConfig{
		Source:  "claim",
		Mapping: map[string][]string{groupEngineering: {"eng@example.com"}, groupSales: {"sales@example.com"}},
	}))

	info := &oauth.UserInfo{Email: "carol@example.com", Claims: map[string]any{"groups": []any{groupEngineering}}}
	plan, err := syncer.Plan(context.Background(), info)
	if err != nil || !slices.Equal(plan.Desired, []string{"eng@example.com"}) {
		t.Fatalf("claim plan = %+v, %v", plan, err)
	}
	if g.CallCount("check") != 0 {
		t.Error("Graph was called although the groups claim was present")
	}

	// Group overage: Entra omits the claim and points to Graph instead.
	info.Claims = map[string]any{"_claim_names": map[string]any{"groups": "src1"}}
	plan, err = syncer.Plan(context.Background(), info)
	if err != nil || !slices.Equal(plan.Desired, []string{"sales@example.com"}) {
		t.Fatalf("overage plan = %+v, %v", plan, err)
	}
	if g.CallCount("check") != 1 {
		t.Error("overage did not fall back to Graph")
	}
}

func TestParseGroupMapping(t *testing.T) {
	got := config.ParseGroupMapping(" G1 : a@x.com, b@x.com ;bad; G2:c@x.com;")
	if len(got) != 2 || !slices.Equal(got["G1"], []string{"a@x.com", "b@x.com"}) || !slices.Equal(got["G2"], []string{"c@x.com"}) {
		t.Fatalf("mapping = %v", got)
	}
}
