package parser

import (
	"reflect"
	"testing"
)

// TestConfigureFromSetup_Pages verifies ConfigureFromSetup reads the "pages"
// field from the filetype setup map, normalizes it, and assigns it to
// PDFParser.Pages.
func TestConfigureFromSetup_Pages(t *testing.T) {
	t.Run("reads and normalizes pages", func(t *testing.T) {
		p := &PDFParser{}
		setup := map[string]any{
			"pages": []any{
				[]any{float64(1), float64(3)},
				[]any{float64(8), float64(10)},
			},
		}
		p.ConfigureFromSetup(setup)
		want := [][]int{{1, 3}, {8, 10}}
		if !reflect.DeepEqual(p.Pages, want) {
			t.Errorf("Pages = %v, want %v", p.Pages, want)
		}
	})

	t.Run("overlapping ranges merged", func(t *testing.T) {
		p := &PDFParser{}
		setup := map[string]any{
			"pages": []any{
				[]any{float64(1), float64(200)},
				[]any{float64(111), float64(333)},
			},
		}
		p.ConfigureFromSetup(setup)
		want := [][]int{{1, 333}}
		if !reflect.DeepEqual(p.Pages, want) {
			t.Errorf("Pages = %v, want %v", p.Pages, want)
		}
	})

	t.Run("all invalid -> nil", func(t *testing.T) {
		p := &PDFParser{}
		setup := map[string]any{
			"pages": []any{[]any{float64(3), float64(1)}},
		}
		p.ConfigureFromSetup(setup)
		if p.Pages != nil {
			t.Errorf("Pages = %v, want nil", p.Pages)
		}
	})

	t.Run("missing pages key -> nil", func(t *testing.T) {
		p := &PDFParser{}
		setup := map[string]any{"flatten_media_to_text": true}
		p.ConfigureFromSetup(setup)
		if p.Pages != nil {
			t.Errorf("Pages = %v, want nil", p.Pages)
		}
	})

	// Regression guard: reading pages must not break other fields.
	t.Run("other fields still read (no regression)", func(t *testing.T) {
		p := &PDFParser{}
		setup := map[string]any{
			"flatten_media_to_text": true,
			"parse_method":          "DeepDOC",
			"output_format":         "json",
			"pages": []any{
				[]any{float64(1), float64(100)},
			},
		}
		p.ConfigureFromSetup(setup)
		if !p.FlattenMediaToText {
			t.Error("FlattenMediaToText not read")
		}
		if p.ParseMethod != "DeepDOC" {
			t.Errorf("ParseMethod = %q, want DeepDOC", p.ParseMethod)
		}
		if p.OutputFormat != "json" {
			t.Errorf("OutputFormat = %q, want json", p.OutputFormat)
		}
		if want := [][]int{{1, 100}}; !reflect.DeepEqual(p.Pages, want) {
			t.Errorf("Pages = %v, want %v", p.Pages, want)
		}
	})
}

// TestConfigureFromSetup_Zoom verifies ConfigureFromSetup reads the "zoom"
// field and passes it through to the DeepDOC parser config.
func TestConfigureFromSetup_Zoom(t *testing.T) {
	t.Run("positive zoom is read", func(t *testing.T) {
		p := &PDFParser{}
		p.ConfigureFromSetup(map[string]any{"zoom": 5.0})
		if p.Zoom != 5.0 {
			t.Errorf("Zoom = %v, want 5.0", p.Zoom)
		}
	})

	t.Run("zero zoom is ignored", func(t *testing.T) {
		p := &PDFParser{Zoom: 3.0}
		p.ConfigureFromSetup(map[string]any{"zoom": 0.0})
		if p.Zoom != 3.0 {
			t.Errorf("Zoom = %v, want 3.0", p.Zoom)
		}
	})

	t.Run("missing zoom key leaves default", func(t *testing.T) {
		p := NewPDFParser()
		p.ConfigureFromSetup(map[string]any{"parse_method": "deepdoc"})
		if p.Zoom != 0 {
			t.Errorf("Zoom = %v, want 0", p.Zoom)
		}
	})
}
