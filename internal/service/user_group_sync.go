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
	"errors"
	"fmt"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/service/oauth"
	"ragflow/internal/utility"
)

// SyncTeamMemberships applies a group-sync plan to the user's team
// memberships, in one transaction:
//   - every desired team is joined as a normal member (an existing invite or
//     inactive row is upgraded; normal and owner rows are kept as they are);
//   - normal memberships of managed teams that are no longer desired are
//     removed, except for default teams;
//   - owner rows are never changed.
//
// Teams are named by their owner's email; unknown teams are logged and skipped.
func (s *UserService) SyncTeamMemberships(ctx context.Context, user *entity.User, plan *oauth.TeamPlan) error {
	userTenantDAO := dao.NewUserTenantDAO()
	desired := toSet(plan.Desired)
	keep := toSet(plan.Default)
	for t := range desired {
		keep[t] = true
	}

	return dao.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rows, err := userTenantDAO.GetByUserIDAll(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		byTenant := map[string][]*entity.UserTenant{}
		for _, r := range rows {
			byTenant[r.TenantID] = append(byTenant[r.TenantID], r)
		}

		for _, team := range plan.Desired {
			tenantID, ownerID, err := s.teamTenant(ctx, tx, team)
			if err != nil {
				return err
			}
			if tenantID == "" || tenantID == user.ID {
				continue
			}
			existing := byTenant[tenantID]
			if len(existing) == 0 {
				status := "1"
				if err := userTenantDAO.Create(ctx, tx, &entity.UserTenant{
					ID:        utility.GenerateToken(),
					UserID:    user.ID,
					TenantID:  tenantID,
					Role:      TenantRoleNormal,
					InvitedBy: ownerID,
					Status:    &status,
				}); err != nil {
					return fmt.Errorf("add %s to team %s: %w", user.Email, team, err)
				}
				common.Info("Group sync: added to team", zap.String("user_id", user.ID), zap.String("team", team))
				continue
			}
			for _, r := range existing {
				if r.Role == TenantRoleOwner || (r.Role == TenantRoleNormal && r.Status != nil && *r.Status == "1") {
					continue
				}
				status := "1"
				r.Role = TenantRoleNormal
				r.Status = &status
				if err := userTenantDAO.Update(ctx, tx, r); err != nil {
					return fmt.Errorf("upgrade %s in team %s: %w", user.Email, team, err)
				}
				common.Info("Group sync: upgraded to normal member", zap.String("user_id", user.ID), zap.String("team", team))
			}
		}

		for _, team := range plan.Managed {
			if keep[team] {
				continue
			}
			tenantID, _, err := s.teamTenant(ctx, tx, team)
			if err != nil {
				return err
			}
			if tenantID == "" || tenantID == user.ID {
				continue
			}
			for _, r := range byTenant[tenantID] {
				if r.Role != TenantRoleNormal {
					continue
				}
				if err := tx.WithContext(ctx).Unscoped().Delete(&entity.UserTenant{}, "id = ?", r.ID).Error; err != nil {
					return fmt.Errorf("remove %s from team %s: %w", user.Email, team, err)
				}
				common.Info("Group sync: removed from team", zap.String("user_id", user.ID), zap.String("team", team))
			}
		}
		return nil
	})
}

// teamTenant returns the tenant owned by the user with the given email and
// that owner's ID, or "" when there is no such team.
func (s *UserService) teamTenant(ctx context.Context, tx *gorm.DB, ownerEmail string) (tenantID, ownerID string, err error) {
	owner, err := s.userDAO.GetByEmail(ctx, tx, ownerEmail)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.Warn("Group sync: team owner does not exist", zap.String("team", ownerEmail))
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	owned, err := dao.NewUserTenantDAO().GetByUserIDAndRole(ctx, tx, owner.ID, TenantRoleOwner)
	if err != nil {
		return "", "", err
	}
	for _, r := range owned {
		if r.TenantID == owner.ID {
			return r.TenantID, owner.ID, nil
		}
	}
	if len(owned) > 0 {
		return owned[0].TenantID, owner.ID, nil
	}
	common.Warn("Group sync: team owner has no tenant", zap.String("team", ownerEmail))
	return "", "", nil
}

func toSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}
