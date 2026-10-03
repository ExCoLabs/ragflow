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

package service

import (
	"context"
	"net/url"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"ragflow/internal/entity"
	"ragflow/internal/service/oauth"
	"ragflow/internal/utility"
)

// setupSharedTestDB opens a named in-memory SQLite database (shared by all
// pool connections) with the user, tenant and model tables.
func setupSharedTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+url.QueryEscape(t.Name())+"?mode=memory&cache=shared"), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&entity.User{}, &entity.Tenant{}, &entity.UserTenant{}, &entity.File{},
		&entity.TenantModelProvider{}, &entity.TenantModelInstance{}, &entity.TenantModel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pushServiceDB(t, db)
	return db
}

// createTestAccount inserts a user owning its own tenant.
func createTestAccount(t *testing.T, db *gorm.DB, email string) *entity.User {
	t.Helper()
	status := "1"
	user := &entity.User{ID: utility.GenerateToken(), Email: email, Nickname: email, IsActive: "1", Status: &status}
	name := email + "'s Kingdom"
	if err := db.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&entity.Tenant{ID: user.ID, Name: &name, Status: &status}).Error; err != nil {
		t.Fatal(err)
	}
	addMembership(t, db, user.ID, user.ID, TenantRoleOwner, "1")
	return user
}

func addMembership(t *testing.T, db *gorm.DB, userID, tenantID, role, status string) {
	t.Helper()
	if err := db.Create(&entity.UserTenant{ID: utility.GenerateToken(), UserID: userID, TenantID: tenantID,
		Role: role, InvitedBy: tenantID, Status: &status}).Error; err != nil {
		t.Fatal(err)
	}
}

// memberships returns tenant ID -> role for the user (all rows, any status).
func memberships(t *testing.T, db *gorm.DB, userID string) map[string]string {
	t.Helper()
	var rows []entity.UserTenant
	if err := db.Where("user_id = ?", userID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.TenantID] = r.Role + "/" + *r.Status
	}
	return out
}

func TestSyncTeamMemberships(t *testing.T) {
	db := setupSharedTestDB(t)
	user := createTestAccount(t, db, "alice@example.com")
	eng := createTestAccount(t, db, "eng@example.com")           // desired, not a member yet
	sales := createTestAccount(t, db, "sales@example.com")       // desired, pending invite
	legacy := createTestAccount(t, db, "legacy@example.com")     // managed, no longer desired
	owned := createTestAccount(t, db, "owned@example.com")       // managed, user is an owner there
	everyone := createTestAccount(t, db, "everyone@example.com") // default team
	manual := createTestAccount(t, db, "manual@example.com")     // not managed by the sync
	addMembership(t, db, user.ID, sales.ID, TenantRoleInvite, "1")
	addMembership(t, db, user.ID, legacy.ID, TenantRoleNormal, "1")
	addMembership(t, db, user.ID, owned.ID, TenantRoleOwner, "1")
	addMembership(t, db, user.ID, manual.ID, TenantRoleNormal, "1")

	plan := &oauth.TeamPlan{
		Desired: []string{"eng@example.com", "everyone@example.com", "missing@example.com", "sales@example.com"},
		Managed: []string{"eng@example.com", "legacy@example.com", "owned@example.com", "sales@example.com"},
		Default: []string{"everyone@example.com"},
	}
	svc := NewUserService()
	for i := 0; i < 2; i++ { // the second run must be a no-op
		if err := svc.SyncTeamMemberships(context.Background(), user, plan); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got := memberships(t, db, user.ID)
		want := map[string]string{
			user.ID:     "owner/1",
			eng.ID:      "normal/1",
			sales.ID:    "normal/1",
			owned.ID:    "owner/1",
			everyone.ID: "normal/1",
			manual.ID:   "normal/1",
		}
		if len(got) != len(want) {
			t.Fatalf("run %d: memberships = %v, want %v", i, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("run %d: tenant %s = %q, want %q", i, k, got[k], v)
			}
		}
	}

	var added entity.UserTenant
	if err := db.Where("user_id = ? AND tenant_id = ?", user.ID, eng.ID).First(&added).Error; err != nil || added.InvitedBy != eng.ID {
		t.Errorf("new membership invited_by = %q, want the team owner (%v)", added.InvitedBy, err)
	}

	// Default teams are never removed, even when also managed and not desired.
	plan = &oauth.TeamPlan{Managed: []string{"everyone@example.com"}, Default: []string{"everyone@example.com"}}
	if err := svc.SyncTeamMemberships(context.Background(), user, plan); err != nil {
		t.Fatal(err)
	}
	if memberships(t, db, user.ID)[everyone.ID] != "normal/1" {
		t.Error("default team membership was removed")
	}
}
