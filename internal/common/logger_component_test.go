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

package common

import (
	"context"
	"testing"
)

func logFieldMap(ctx context.Context) map[string]string {
	fields := map[string]string{}
	for _, field := range sessionFields(ctx) {
		fields[field.Key] = field.String
	}
	return fields
}

// Ctx-variant log lines carry the session and, when a canvas component is
// running, which one — a sub-agent as "<component>/<sub-agent>".
func TestSessionFieldsCarryTheComponent(t *testing.T) {
	if got := sessionFields(t.Context()); got != nil {
		t.Fatalf("bare context fields = %v", got)
	}
	ctx := WithSessionID(t.Context(), "sess-1")
	if got := logFieldMap(ctx); len(got) != 1 || got["session_id"] != "sess-1" {
		t.Fatalf("session only = %v", got)
	}
	ctx = WithLogComponent(ctx, "Agent:Abc")
	ctx = WithLogComponent(ctx, " ")
	child := WithLogComponent(ctx, LogComponentFromContext(ctx)+"/navigo_agent")
	if got := logFieldMap(ctx); got["component"] != "Agent:Abc" || got["session_id"] != "sess-1" {
		t.Fatalf("component fields = %v", got)
	}
	if got := logFieldMap(child); got["component"] != "Agent:Abc/navigo_agent" {
		t.Fatalf("sub-agent fields = %v", got)
	}
}
