package common

import (
	"reflect"
	"testing"
)

func TestDocVersionsPreserveUnverifiedContent(t *testing.T) {
	cases := []struct {
		name   string
		chunks []map[string]any
	}{
		{"q25_upload_order", []map[string]any{
			{"doc_id": "current", "docnm_kwd": "010_G00_DSP_E1_10_STA_TSP_002_R00(9).pdf", "content_with_weight": "Otvory do 200 mm"},
			{"doc_id": "other", "docnm_kwd": "010_G00_DSP_E1_10_STA_TSP_002_R00(10).pdf", "content_with_weight": "Other structural report"},
		}},
		{"different_hashes", []map[string]any{
			{"doc_id": "a", "docnm_kwd": "project/sp/report(1).pdf", "kb_id": "kb", "document_content_hash": "aaa", "content_with_weight": "same evidence"},
			{"doc_id": "b", "docnm_kwd": "project/sp/report(2).pdf", "kb_id": "kb", "document_content_hash": "bbb", "content_with_weight": "same evidence"},
		}},
		{"missing_path", []map[string]any{
			{"doc_id": "a", "docnm_kwd": "report(1).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
			{"doc_id": "b", "docnm_kwd": "report(2).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
		}},
		{"different_scope", []map[string]any{
			{"doc_id": "a", "docnm_kwd": "project/sp/report(1).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
			{"doc_id": "b", "docnm_kwd": "project/rp/report(2).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
		}},
		{"different_dataset", []map[string]any{
			{"doc_id": "a", "docnm_kwd": "project/sp/report(1).pdf", "kb_id": "kb1", "document_content_hash": "same", "content_with_weight": "same evidence"},
			{"doc_id": "b", "docnm_kwd": "project/sp/report(2).pdf", "kb_id": "kb2", "document_content_hash": "same", "content_with_weight": "same evidence"},
		}},
		{"different_chunk_content", []map[string]any{
			{"doc_id": "a", "docnm_kwd": "project/sp/report(1).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "Openings 200 mm"},
			{"doc_id": "b", "docnm_kwd": "project/sp/report(2).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "Other paragraph"},
		}},
		{"chunk_hash_is_not_document_identity", []map[string]any{
			{"doc_id": "a", "docnm_kwd": "project/sp/report(1).pdf", "kb_id": "kb", "content_hash": "same", "content_with_weight": "same evidence"},
			{"doc_id": "b", "docnm_kwd": "project/sp/report(2).pdf", "kb_id": "kb", "content_hash": "same", "content_with_weight": "same evidence"},
		}},
		{"dwg_is_not_a_copy", []map[string]any{
			{"doc_id": "a", "docnm_kwd": "report - titulka.dwg(5).md", "content_with_weight": "Drawing evidence"},
			{"doc_id": "b", "docnm_kwd": "report.pdf", "content_with_weight": "Different evidence"},
		}},
		{"missing_identity", []map[string]any{
			{"docnm_kwd": "report(1).pdf"}, {"docnm_kwd": "report(2).pdf"}, {},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreferNewestDocVersions(tc.chunks); !reflect.DeepEqual(got, tc.chunks) {
				t.Fatalf("different or unverified documents were dropped: got %v, want %v", got, tc.chunks)
			}
			field := make(map[string]map[string]any)
			ids := []string{"first", "second"}
			for i, id := range ids {
				field[id] = tc.chunks[i]
			}
			if got := PreferNewestDocVersionIDs(ids, field); !reflect.DeepEqual(got, ids) {
				t.Fatalf("candidate documents were dropped: %v", got)
			}
		})
	}
}

func TestDocVersionsPreserveSourceDirectories(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, dataset string
	}{
		{"case_sensitive_directories", "Project/SP/report(1).pdf", "project/sp/report(2).pdf", "kb"},
		{"dwg_in_directory", "project.dwg-a/sp/one(1).pdf", "project.dwg-b/sp/two(2).pdf", "kb"},
		{"extensionless_sources", "project.v1/sp/one(1)", "project.v2/sp/two(2)", "kb"},
		{"missing_filename", "project/report(1).pdf/", "project/report(2).pdf/", "kb"},
		{"blank_dataset", "project/sp/report(1).pdf", "project/sp/report(2).pdf", " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := []map[string]any{
				{"doc_id": "a", "docnm_kwd": tc.first, "kb_id": tc.dataset, "document_content_hash": "same", "content_with_weight": "same text"},
				{"doc_id": "b", "docnm_kwd": tc.second, "kb_id": tc.dataset, "document_content_hash": "same", "content_with_weight": "same text"},
			}
			if got := PreferNewestDocVersions(chunks); !reflect.DeepEqual(got, chunks) {
				t.Errorf("result-level helper dropped a distinct or unverified source: %v", got)
			}
			ids := []string{"a", "b"}
			if got := PreferNewestDocVersionIDs(ids, map[string]map[string]any{"a": chunks[0], "b": chunks[1]}); !reflect.DeepEqual(got, ids) {
				t.Errorf("candidate-level helper returned %v, want %v", got, ids)
			}
		})
	}
}

func TestDocVersionsRankOnlyUploadSuffix(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"numbered_directory", "project(2024)/sp/report(1).pdf", "project(2024)/sp/report(2).pdf"},
		{"numbered_stem", "project/sp/report(2024)(1).pdf", "project/sp/report(2024)(2).pdf"},
		{"suffix_after_extension", "project/sp/report(2024).pdf(1)", "project/sp/report(2024).pdf(2)"},
		{"dwg_suffix", "project/sp/report(2024).dwg(1)", "project/sp/report(2024).dwg(2)"},
		{"windows_path", `project(2024)\sp\report(1).pdf`, `project(2024)\sp\report(2).pdf`},
		{"extensionless_copies", "project.v1/sp/report(1)", "project.v1/sp/report(2)"},
		{"mixed_separators", `project\sp\report(1).pdf`, "project/sp/report(2).pdf"},
		{"dwg_directory_does_not_change_file_type", "project.dwg/sp/report - title.dwg(9).md", "project.dwg/sp/report(1).pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := []map[string]any{
				{"doc_id": "a", "docnm_kwd": tc.first, "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same text"},
				{"doc_id": "b", "docnm_kwd": tc.second, "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same text"},
			}
			if got := PreferNewestDocVersions(chunks); !reflect.DeepEqual(got, chunks[1:]) {
				t.Errorf("result-level preference = %v, want %v", got, chunks[1:])
			}
			if got := PreferNewestDocVersionIDs([]string{"a", "b"}, map[string]map[string]any{"a": chunks[0], "b": chunks[1]}); !reflect.DeepEqual(got, []string{"b"}) {
				t.Errorf("candidate-level preference = %v, want [b]", got)
			}
		})
	}
}

func TestDocVersionsCollapseOnlyVerifiedCopies(t *testing.T) {
	chunks := []map[string]any{
		{"doc_id": "a", "docnm_kwd": "project/sp/report(1).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
		{"doc_id": "b", "docnm_kwd": "project/sp/report(2).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
		{"doc_id": "b", "docnm_kwd": "project/sp/report(2).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
	}
	want := chunks[1:]
	if got := PreferNewestDocVersions(chunks); !reflect.DeepEqual(got, want) {
		t.Fatalf("verified copy preference = %v, want %v", got, want)
	}
	field := map[string]map[string]any{"first": chunks[0], "second": chunks[1], "third": chunks[2]}
	if got := PreferNewestDocVersionIDs([]string{"first", "second", "third", "missing"}, field); !reflect.DeepEqual(got, []string{"second", "third", "missing"}) {
		t.Fatalf("verified candidate copies = %v", got)
	}
}
