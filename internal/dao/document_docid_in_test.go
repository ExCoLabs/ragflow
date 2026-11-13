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

package dao

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"ragflow/internal/entity"
)

func docIDInStatement(t *testing.T, docIDs []string) *gorm.Statement {
	t.Helper()
	db := setupDocumentTestDB(t).Session(&gorm.Session{DryRun: true})
	var docs []entity.Document
	return whereDocIDIn(db.Model(&entity.Document{}), "id", docIDs).Find(&docs).Statement
}

func TestWhereDocIDIn_SmallListUsesPlaceholders(t *testing.T) {
	stmt := docIDInStatement(t, []string{"a1", "b2"})
	if len(stmt.Vars) != 2 {
		t.Fatalf("expected 2 bound vars, got %d: %s", len(stmt.Vars), stmt.SQL.String())
	}
}

func TestWhereDocIDIn_LargeListStaysUnderPlaceholderLimit(t *testing.T) {
	ids := make([]string, 70000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i)
	}
	stmt := docIDInStatement(t, ids)
	if len(stmt.Vars) != 0 {
		t.Fatalf("expected the IN list inlined, got %d bound vars", len(stmt.Vars))
	}
}

func TestWhereDocIDIn_LargeListWithUnsafeIDKeepsPlaceholders(t *testing.T) {
	ids := make([]string, maxDocIDPlaceholders+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i)
	}
	ids[0] = "x') OR ('1'='1"
	stmt := docIDInStatement(t, ids)
	if len(stmt.Vars) != len(ids) {
		t.Fatalf("expected unsafe IDs bound, got %d vars", len(stmt.Vars))
	}
}
