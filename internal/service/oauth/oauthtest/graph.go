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

package oauthtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const graphToken = "test-graph-token"

// Graph is a fake of the Microsoft Graph endpoints used by group sync: the
// client-credentials token endpoint, checkMemberGroups and the group lookup
// by display name.
type Graph struct {
	Server *httptest.Server

	mu sync.Mutex
	// Members maps a user (email) to the IDs of the groups they belong to.
	// Users missing from the map get a 404 like unknown directory users.
	Members map[string][]string
	// Groups maps display names to group IDs.
	Groups map[string]string
	// Calls counts requests per kind: "token", "check", "lookup".
	Calls map[string]int
}

// NewGraph starts a fake Graph; it is closed when the test ends.
func NewGraph(t *testing.T) *Graph {
	t.Helper()
	g := &Graph{Members: map[string][]string{}, Groups: map[string]string{}, Calls: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", g.token)
	mux.HandleFunc("/v1.0/", g.api)
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Server.Close)
	return g
}

// URL is the Graph API base (graph_url).
func (g *Graph) URL() string { return g.Server.URL + "/v1.0" }

// TokenURL is the token endpoint (graph_token_url).
func (g *Graph) TokenURL() string { return g.Server.URL + "/token" }

// CallCount returns how often an endpoint kind was called.
func (g *Graph) CallCount(kind string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Calls[kind]
}

func (g *Graph) token(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.Calls["token"]++
	g.mu.Unlock()
	_ = r.ParseForm()
	if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_id") != ClientID ||
		r.PostForm.Get("client_secret") != ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": graphToken, "token_type": "Bearer", "expires_in": 3600})
}

func (g *Graph) api(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+graphToken {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.0")
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/users/") && strings.HasSuffix(path, "/checkMemberGroups"):
		g.Calls["check"]++
		user := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/checkMemberGroups")
		groups, ok := g.Members[user]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		var body struct {
			GroupIDs []string `json:"groupIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		member := map[string]bool{}
		for _, id := range groups {
			member[id] = true
		}
		out := []string{}
		for _, id := range body.GroupIDs {
			if member[id] {
				out = append(out, id)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"value": out})
	case r.Method == http.MethodGet && path == "/groups":
		g.Calls["lookup"]++
		filter := r.URL.Query().Get("$filter")
		name := strings.TrimSuffix(strings.TrimPrefix(filter, "displayName eq '"), "'")
		value := []map[string]any{}
		for n, id := range g.Groups {
			if strings.EqualFold(n, name) {
				value = append(value, map[string]any{"id": id, "displayName": n})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"value": value})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}
