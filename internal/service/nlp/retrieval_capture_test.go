package nlp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ragflow/internal/common"
)

func TestRetrievalCandidateCapture(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_DIR", dir)
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_SESSION", "session-1")
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_TENANT", "tenant-1")
	req := &RetrievalRequest{Question: "private question", TenantIDs: []string{"tenant-1"}}
	result := &RetrievalSearchResult{IDs: []string{"second", "first"}, Field: map[string]map[string]interface{}{"second": {"content_with_weight": "private evidence"}, "first": {"meta_project_id_kwd": "2018_055"}}}
	for _, tc := range []struct {
		name, session string
		tenants       []string
		want          bool
	}{
		{"missing session", "", []string{"tenant-1"}, false},
		{"other session", "session-2", []string{"tenant-1"}, false},
		{"other tenant", "session-1", []string{"tenant-2"}, false},
		{"multiple tenants", "session-1", []string{"tenant-1", "tenant-2"}, false},
		{"selected request", "session-1", []string{"tenant-1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req.TenantIDs = tc.tenants
			captured, err := captureRetrievalCandidates(common.WithSessionID(context.Background(), tc.session), req, result)
			if err != nil || captured != tc.want {
				t.Fatalf("captured=%v err=%v", captured, err)
			}
		})
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	path := filepath.Join(dir, entries[0].Name())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot retrievalCandidateSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Result.IDs[0] != "second" || snapshot.Result.IDs[1] != "first" {
		t.Fatalf("snapshot did not preserve candidate order: %v", snapshot.Result.IDs)
	}
	if snapshot.Result.Field["second"]["content_with_weight"] != "private evidence" {
		t.Fatal("snapshot lost candidate content")
	}
	if snapshot.Request.Question != req.Question || snapshot.Stage != "post_prune_pre_score" {
		t.Fatalf("unexpected snapshot metadata: %#v", snapshot)
	}
	for _, field := range []string{"APIKey", "EmbeddingModel", "RerankModel", "ModelDriver"} {
		if data, _ := json.Marshal(snapshot.Request); strings.Contains(string(data), field) {
			t.Fatalf("snapshot serialized sensitive field %s", field)
		}
	}
}

func TestRetrievalCandidateCaptureDisabled(t *testing.T) {
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_DIR", "")
	captured, err := captureRetrievalCandidates(context.Background(), nil, nil)
	if captured || err != nil {
		t.Fatalf("captured=%v err=%v", captured, err)
	}
}

func TestRetrievalCandidateCaptureRejectsUnsafeStorage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_DIR", dir)
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_SESSION", "selected")
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_TENANT", "tenant")
	ctx := common.WithSessionID(context.Background(), "selected")
	req := &RetrievalRequest{TenantIDs: []string{"tenant"}}
	for _, mode := range []os.FileMode{0755, 0770} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := captureRetrievalCandidates(ctx, req, &RetrievalSearchResult{}); err == nil {
			t.Fatalf("accepted mode %v", mode)
		}
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_DIR", link)
	if _, err := captureRetrievalCandidates(ctx, req, &RetrievalSearchResult{}); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestRetrievalCandidateCaptureBounds(t *testing.T) {
	writer := &captureBuffer{limit: 4}
	if _, err := writer.Write([]byte("12345")); err == nil || writer.Len() != 0 {
		t.Fatal("oversized write accepted")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_DIR", dir)
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_SESSION", "selected")
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_TENANT", "tenant")
	ctx := common.WithSessionID(context.Background(), "selected")
	req := &RetrievalRequest{TenantIDs: []string{"tenant"}}
	for i := 0; i < maxRetrievalCaptures; i++ {
		if _, err := captureRetrievalCandidates(ctx, req, &RetrievalSearchResult{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := captureRetrievalCandidates(ctx, req, &RetrievalSearchResult{}); err == nil {
		t.Fatal("quota exceeded")
	}
}

func TestRetrievalCandidateCaptureFailureCleanup(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_DIR", dir)
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_SESSION", "selected")
	t.Setenv("RAGFLOW_RETRIEVAL_CAPTURE_TENANT", "tenant")
	ctx := common.WithSessionID(context.Background(), "selected")
	req := &RetrievalRequest{TenantIDs: []string{"tenant"}}
	result := &RetrievalSearchResult{Field: map[string]map[string]interface{}{"bad": {"unsupported": make(chan int)}}}
	if captured, err := captureRetrievalCandidates(ctx, req, result); captured || err == nil {
		t.Fatalf("serialization failure captured=%v err=%v", captured, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failure left files: %v err=%v", entries, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if captured, err := captureRetrievalCandidates(cancelled, req, &RetrievalSearchResult{}); captured || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture=%v err=%v", captured, err)
	}
	if captured, err := captureRetrievalCandidates(ctx, req, &RetrievalSearchResult{}); !captured || err != nil {
		t.Fatalf("capture failed after cleanup: %v %v", captured, err)
	}
}
