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
	"slices"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/server/config"
)

// defaultModelMaxTokens is used for user_default_llm models without max_tokens
// (the same default as adding a model through the API).
const defaultModelMaxTokens = 8192

// userLLMInstance is one provider instance to create for user_default_llm:
// the models sharing a factory, API key and base URL.
type userLLMInstance struct {
	factory, apiKey, baseURL, name string
	models                         []CreateInstanceModelInfo
}

// ProvisionDefaultModels gives the tenant owned by userID the provider,
// instance(s) and models of the `user_default_llm` config and makes the
// configured default models the tenant defaults. It is idempotent: existing
// providers, instances and models are reused.
func ProvisionDefaultModels(ctx context.Context, userID string, cfg config.UserDefaultLLMConfig) error {
	if len(cfg.Models) == 0 {
		return nil
	}
	instances, defaults := planUserLLMInstances(cfg)
	providers := NewModelProviderService()
	var errs []error
	for _, inst := range instances {
		if err := ensureUserLLMInstance(ctx, providers, userID, inst); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", inst.factory, inst.name, err))
		}
	}
	tenants := NewTenantService()
	for _, d := range defaults {
		if err := tenants.SetTenantDefaultModels(ctx, userID, d.factory, d.instance, d.name, d.typ, ""); err != nil {
			errs = append(errs, fmt.Errorf("default %s model %s: %w", d.typ, d.name, err))
		}
	}
	return errors.Join(errs...)
}

type userLLMDefault struct{ typ, factory, instance, name string }

// planUserLLMInstances groups the configured models into instances. Models
// using the top-level factory / api_key / base_url go to instance_name; a
// model with its own credentials gets its own instance, named after its type.
func planUserLLMInstances(cfg config.UserDefaultLLMConfig) ([]*userLLMInstance, []userLLMDefault) {
	var instances []*userLLMInstance
	byKey := map[string]*userLLMInstance{}
	usedNames := map[string]bool{}
	var defaults []userLLMDefault

	for _, m := range cfg.Models {
		if m.Factory == "" {
			common.Warn("user_default_llm: skipping model without factory", zap.String("model", m.Name))
			continue
		}
		key := m.Factory + "\x00" + m.APIKey + "\x00" + m.BaseURL
		inst := byKey[key]
		if inst == nil {
			name := cfg.InstanceName
			if m.Factory != cfg.Factory || m.APIKey != cfg.APIKey || m.BaseURL != cfg.BaseURL || usedNames[name] {
				name = cfg.InstanceName + "-" + m.Type
				for i := 2; usedNames[name]; i++ {
					name = fmt.Sprintf("%s-%s-%d", cfg.InstanceName, m.Type, i)
				}
			}
			usedNames[name] = true
			inst = &userLLMInstance{factory: m.Factory, apiKey: m.APIKey, baseURL: m.BaseURL, name: name}
			byKey[key] = inst
			instances = append(instances, inst)
		}

		maxTokens := m.MaxTokens
		if maxTokens <= 0 {
			maxTokens = defaultModelMaxTokens
		}
		idx := slices.IndexFunc(inst.models, func(x CreateInstanceModelInfo) bool { return x.ModelName == m.Name })
		if idx < 0 {
			inst.models = append(inst.models, CreateInstanceModelInfo{ModelName: m.Name, MaxTokens: maxTokens})
			idx = len(inst.models) - 1
		}
		model := &inst.models[idx]
		if !slices.Contains(model.ModelTypes, m.Type) {
			model.ModelTypes = append(model.ModelTypes, m.Type)
		}
		model.MaxTokens = max(model.MaxTokens, maxTokens)
		if m.IsTools != nil {
			if model.Extra == nil {
				model.Extra = map[string]interface{}{}
			}
			model.Extra["is_tools"] = *m.IsTools
		}
		if m.Default {
			defaults = append(defaults, userLLMDefault{typ: m.Type, factory: m.Factory, instance: inst.name, name: m.Name})
		}
	}
	return instances, defaults
}

func ensureUserLLMInstance(ctx context.Context, providers *ModelProviderService, userID string, inst *userLLMInstance) error {
	if _, err := providers.AddModelProvider(ctx, inst.factory, userID); err != nil {
		return fmt.Errorf("add provider: %w", err)
	}
	owned, err := providers.userTenantDAO.GetByUserIDAndRole(ctx, dao.DB, userID, "owner")
	if err != nil || len(owned) == 0 {
		return fmt.Errorf("user has no tenant: %w", err)
	}
	provider, err := providers.modelProviderDAO.GetByTenantIDAndProviderName(ctx, dao.DB, owned[0].TenantID, inst.factory)
	if err != nil {
		return err
	}
	_, err = providers.modelInstanceDAO.GetByProviderIDAndInstanceName(ctx, dao.DB, provider.ID, inst.name)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if _, err := providers.CreateProviderInstance(ctx, inst.factory, inst.name, inst.apiKey, inst.baseURL, "", userID, inst.models); err != nil {
			return fmt.Errorf("create instance: %w", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	for _, m := range inst.models {
		code, err := providers.AddModel(ctx, &AddModelRequest{
			ProviderName: inst.factory,
			InstanceName: inst.name,
			ModelName:    m.ModelName,
			ModelTypes:   m.ModelTypes,
			MaxTokens:    m.MaxTokens,
			Extra:        m.Extra,
		}, userID)
		if err != nil && code != common.CodeConflict {
			return fmt.Errorf("add model %s: %w", m.ModelName, err)
		}
	}
	return nil
}
