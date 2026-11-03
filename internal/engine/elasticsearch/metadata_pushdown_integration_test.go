//go:build integration

package elasticsearch

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"ragflow/internal/entity"
)

func TestMetadataPushdownLargeExistingScope(t *testing.T) {
	var probe struct {
		TenantID string `json:"tenant_id"`
		KBID     string `json:"kb_id"`
		Project  string `json:"project"`
	}
	if os.Getenv("ES_METADATA_PROBE") == "" {
		t.Skip("set ES_METADATA_PROBE to run the read-only large-scope check")
	}
	if err := json.Unmarshal([]byte(os.Getenv("ES_METADATA_PROBE")), &probe); err != nil || probe.TenantID == "" || probe.KBID == "" || probe.Project == "" {
		t.Fatal("invalid metadata probe configuration")
	}
	client, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses: []string{os.Getenv("ES_HOST")},
		Username:  os.Getenv("ES_USERNAME"), Password: os.Getenv("ES_PASSWORD"),
	})
	if err != nil {
		t.Fatal("failed to create Elasticsearch client")
	}
	eng := &Engine{client: client}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&entity.Knowledgebase{}); err != nil {
		t.Fatal(err)
	}
	status := string(entity.StatusValid)
	if err := db.Create(&entity.Knowledgebase{ID: probe.KBID, TenantID: probe.TenantID, Status: &status}).Error; err != nil {
		t.Fatal(err)
	}
	conditions := []map[string]interface{}{{"key": "project", "op": "contains", "value": probe.Project}}
	query, err := BuildMetaFilterQuery(conditions, "and", []string{probe.KBID})
	if err != nil {
		t.Fatal("failed to build scoped metadata query")
	}
	body, _ := json.Marshal(query)
	res, err := client.Count(client.Count.WithContext(t.Context()), client.Count.WithIndex(buildMetadataIndexName(probe.TenantID)), client.Count.WithBody(bytes.NewReader(body)))
	if err != nil {
		t.Fatal("metadata count request failed")
	}
	defer res.Body.Close()
	var expected struct {
		Count int `json:"count"`
	}
	if res.IsError() || json.NewDecoder(res.Body).Decode(&expected) != nil || expected.Count <= metaPushdownMaxSize {
		t.Fatal("probe must select more than one metadata result window")
	}
	ids := eng.FilterDocIdsByMetaPushdown(t.Context(), db, []string{probe.KBID}, conditions, "and")
	if len(ids) != expected.Count {
		t.Fatalf("got %d IDs, want complete scope of %d", len(ids), expected.Count)
	}
	t.Logf("complete metadata scope: %d documents", len(ids))
}
