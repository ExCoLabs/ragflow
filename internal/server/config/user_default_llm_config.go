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

package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// DefaultUserLLMInstanceName is the provider instance created for new
// tenants when user_default_llm sets no instance_name.
const DefaultUserLLMInstanceName = "default-llm"

// UserDefaultLLMModel is one resolved entry of user_default_llm.default_models
// (or extra_models). Factory, APIKey and BaseURL fall back to the top-level
// values of user_default_llm.
type UserDefaultLLMModel struct {
	// Type is the model type: chat, embedding, image2text, rerank, asr, tts.
	Type      string
	Name      string
	Factory   string
	APIKey    string
	BaseURL   string
	MaxTokens int
	// IsTools marks a chat model as tool-calling capable (nil = not set).
	IsTools *bool
	// Default marks the tenant default model of Type.
	Default bool
}

// UserDefaultLLMConfig is the `user_default_llm` section: the model provider
// and models every new tenant is created with.
type UserDefaultLLMConfig struct {
	Factory      string
	APIKey       string
	BaseURL      string
	InstanceName string
	Models       []UserDefaultLLMModel
}

// defaultModelKeys maps the keys of user_default_llm.default_models to model
// types. vision_model is what the Python settings read; image2text_model is
// what the template documented.
var defaultModelKeys = []struct{ key, typ string }{
	{"chat_model", "chat"},
	{"embedding_model", "embedding"},
	{"image2text_model", "image2text"},
	{"vision_model", "image2text"},
	{"rerank_model", "rerank"},
	{"asr_model", "asr"},
	{"tts_model", "tts"},
}

// ParseUserDefaultLLMConfig reads `user_default_llm`. A model entry is a model
// name (optionally "name@factory") or a map with name (alias model), factory,
// api_key, base_url, max_tokens and is_tools.
func (c *Config) ParseUserDefaultLLMConfig(v *viper.Viper) error {
	c.userDefaultLLM = UserDefaultLLMConfig{}
	if !v.IsSet("user_default_llm") {
		return nil
	}
	sub := v.Sub("user_default_llm")
	if sub == nil {
		return nil
	}
	cfg := UserDefaultLLMConfig{
		Factory:      strings.TrimSpace(sub.GetString("factory")),
		APIKey:       strings.TrimSpace(sub.GetString("api_key")),
		BaseURL:      strings.TrimSpace(sub.GetString("base_url")),
		InstanceName: strings.TrimSpace(sub.GetString("instance_name")),
	}
	if cfg.InstanceName == "" {
		cfg.InstanceName = DefaultUserLLMInstanceName
	}

	defaults, _ := sub.Get("default_models").(map[string]any)
	seenType := map[string]bool{}
	for _, k := range defaultModelKeys {
		raw, ok := defaults[k.key]
		if !ok || seenType[k.typ] {
			continue
		}
		m, err := parseUserDefaultLLMModel(raw, k.typ, cfg)
		if err != nil {
			return fmt.Errorf("user_default_llm.default_models.%s: %w", k.key, err)
		}
		if m.Name == "" {
			continue
		}
		m.Default = true
		seenType[k.typ] = true
		cfg.Models = append(cfg.Models, m)
	}

	extras, _ := sub.Get("extra_models").([]any)
	for i, raw := range extras {
		entry, _ := raw.(map[string]any)
		typ := strings.TrimSpace(fmt.Sprint(entry["type"]))
		if entry == nil || typ == "" || typ == "<nil>" {
			return fmt.Errorf("user_default_llm.extra_models[%d]: needs a type", i)
		}
		m, err := parseUserDefaultLLMModel(raw, typ, cfg)
		if err != nil {
			return fmt.Errorf("user_default_llm.extra_models[%d]: %w", i, err)
		}
		if m.Name != "" {
			cfg.Models = append(cfg.Models, m)
		}
	}
	c.userDefaultLLM = cfg
	return nil
}

func parseUserDefaultLLMModel(raw any, typ string, base UserDefaultLLMConfig) (UserDefaultLLMModel, error) {
	m := UserDefaultLLMModel{Type: typ}
	switch v := raw.(type) {
	case nil:
	case string:
		m.Name = strings.TrimSpace(v)
	case map[string]any:
		str := func(key string) string {
			if s, ok := v[key].(string); ok {
				return strings.TrimSpace(s)
			}
			return ""
		}
		m.Name = str("name")
		if m.Name == "" {
			m.Name = str("model")
		}
		m.Factory, m.APIKey, m.BaseURL = str("factory"), str("api_key"), str("base_url")
		switch n := v["max_tokens"].(type) {
		case int:
			m.MaxTokens = n
		case float64:
			m.MaxTokens = int(n)
		}
		if b, ok := v["is_tools"].(bool); ok {
			m.IsTools = &b
		}
	default:
		return m, fmt.Errorf("unsupported value %T", raw)
	}
	// "name@factory" shorthand, as the Python settings accepted.
	if name, factory, ok := strings.Cut(m.Name, "@"); ok && m.Factory == "" && !strings.Contains(factory, "@") {
		m.Name, m.Factory = name, factory
	}
	if m.Factory == "" {
		m.Factory = base.Factory
	}
	if m.APIKey == "" {
		m.APIKey = base.APIKey
	}
	if m.BaseURL == "" {
		m.BaseURL = base.BaseURL
	}
	return m, nil
}

// GetUserDefaultLLM returns the parsed user_default_llm section.
func (c *Config) GetUserDefaultLLM() UserDefaultLLMConfig {
	return c.userDefaultLLM
}
