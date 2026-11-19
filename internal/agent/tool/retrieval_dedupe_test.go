package tool

import "testing"

func TestCapChunksPerDocument(t *testing.T) {
	chunks := []RetrievalChunk{
		{ID: "a1", DocumentID: "d1", DocumentName: "SPEC.pdf"},
		{ID: "a2", DocumentID: "d1", DocumentName: "SPEC.pdf"},
		{ID: "b1", DocumentID: "d2", DocumentName: "SPEC(3).pdf"},
		{ID: "c1", DocumentID: "d3", DocumentName: "other.md"},
		{ID: "a3", DocumentID: "d1", DocumentName: "SPEC.pdf"},
		{ID: "b2", DocumentID: "d2", DocumentName: "SPEC(3).pdf"},
	}
	got := capChunksPerDocument(chunks, maxChunksPerDocument)
	ids := []string{}
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	want := []string{"a1", "a2", "c1"} // (N) variants share the base document
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("got %v, want %v", ids, want)
		}
	}
}
