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
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestParseUserDefaultLLMConfig(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
user_default_llm:
  factory: OpenAI-API-Compatible
  api_key: base-key
  base_url: https://llm.example.com/v1
  default_models:
    chat_model:
      name: chat-1
      max_tokens: 32768
      is_tools: true
    embedding_model: embed-1
    vision_model:
      model: vl-1
    rerank_model: rerank-1@Jina
    asr_model:
      name: whisper
      api_key: asr-key
      base_url: https://asr.example.com
  extra_models:
    - type: chat
      name: chat-2
`)); err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := c.ParseUserDefaultLLMConfig(v); err != nil {
		t.Fatal(err)
	}
	got := c.GetUserDefaultLLM()
	if got.InstanceName != DefaultUserLLMInstanceName || len(got.Models) != 6 {
		t.Fatalf("config = %+v", got)
	}
	byType := map[string]UserDefaultLLMModel{}
	for _, m := range got.Models {
		if m.Default {
			byType[m.Type] = m
		}
	}
	chat := byType["chat"]
	if chat.Name != "chat-1" || chat.MaxTokens != 32768 || chat.IsTools == nil || !*chat.IsTools ||
		chat.Factory != "OpenAI-API-Compatible" || chat.APIKey != "base-key" || chat.BaseURL != "https://llm.example.com/v1" {
		t.Errorf("chat = %+v", chat)
	}
	if byType["embedding"].Name != "embed-1" || byType["image2text"].Name != "vl-1" {
		t.Errorf("embedding/vision = %+v / %+v", byType["embedding"], byType["image2text"])
	}
	if r := byType["rerank"]; r.Name != "rerank-1" || r.Factory != "Jina" {
		t.Errorf("rerank shorthand = %+v", r)
	}
	if a := byType["asr"]; a.APIKey != "asr-key" || a.BaseURL != "https://asr.example.com" || a.Factory != "OpenAI-API-Compatible" {
		t.Errorf("asr overrides = %+v", a)
	}
	if extra := got.Models[5]; extra.Default || extra.Type != "chat" || extra.Name != "chat-2" {
		t.Errorf("extra model = %+v", extra)
	}

	// The shipped template only sets credentials, no model: nothing to provision.
	v = viper.New()
	v.Set("user_default_llm", map[string]any{"default_models": map[string]any{"embedding_model": map[string]any{"api_key": "xxx"}}})
	if err := c.ParseUserDefaultLLMConfig(v); err != nil || len(c.GetUserDefaultLLM().Models) != 0 {
		t.Fatalf("template config: %+v, %v", c.GetUserDefaultLLM(), err)
	}
}
