package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactConfigValueHidesSecretsAtAnyDepth(t *testing.T) {
	settings := map[string]interface{}{
		"mysql": map[string]interface{}{"host": "mysql", "password": "db-pw", "port": 3306},
		"oauth": map[string]interface{}{
			"entra": map[string]interface{}{"client_id": "app-id", "client_secret": "entra-secret", "issuer": "https://issuer"},
		},
		"user_default_llm": map[string]interface{}{
			"api_key":        "llm-key",
			"default_models": map[string]interface{}{"chat_model": map[string]interface{}{"name": "m", "max_tokens": 8192, "api_key": "chat-key"}},
		},
		"s3":    map[string]interface{}{"access_key": "ak", "secret_key": "sk", "region_name": "gra"},
		"redis": map[string]interface{}{"password": ""},
		"list":  []interface{}{map[string]interface{}{"token": "t1"}},
	}

	raw, err := json.Marshal(redactConfigValue("", settings))
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, secret := range []string{"db-pw", "entra-secret", "llm-key", "chat-key", "\"ak\"", "\"sk\"", "t1"} {
		if strings.Contains(out, secret) {
			t.Errorf("redacted config still contains %s: %s", secret, out)
		}
	}
	for _, kept := range []string{"app-id", "https://issuer", "\"max_tokens\":8192", "\"port\":3306", "gra", "\"password\":\"\""} {
		if !strings.Contains(out, kept) {
			t.Errorf("redacted config lost %s: %s", kept, out)
		}
	}
	if settings["mysql"].(map[string]interface{})["password"] != "db-pw" {
		t.Error("redactConfigValue modified the live settings map")
	}
}
