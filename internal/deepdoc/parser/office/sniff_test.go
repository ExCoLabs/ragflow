// Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package office

import (
	"archive/zip"
	"bytes"
	"hash/crc32"
	"io"
	"testing"
)

// testZip writes sizes into the local headers, as Word does; zip.Writer.Create
// would defer them to data descriptors.
func testZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name:               name,
			Method:             zip.Store,
			CRC32:              crc32.ChecksumIEEE([]byte(body)),
			CompressedSize64:   uint64(len(body)),
			UncompressedSize64: uint64(len(body)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestNormalizeInputFormatFromMagic(t *testing.T) {
	zipData := testZip(t, map[string]string{"word/document.xml": "<w/>"})
	ole := append(append([]byte{}, oleMagic...), make([]byte, 64)...)
	cases := []struct {
		name       string
		data       []byte
		format     string
		wantFormat string
	}{
		{"docx saved as doc", zipData, "doc", "docx"},
		{"xlsx saved as xls", zipData, "xls", "xlsx"},
		{"pptx saved as ppt", zipData, "ppt", "pptx"},
		{"real docx", zipData, "docx", "docx"},
		{"doc saved as docx", ole, "docx", "doc"},
		{"real doc", ole, "doc", "doc"},
		{"unknown bytes keep extension", []byte("plain text"), "doc", "doc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := normalizeInput(tc.data, tc.format); got != tc.wantFormat {
				t.Errorf("format = %q, want %q", got, tc.wantFormat)
			}
		})
	}
}

func TestNormalizeInputSalvagesTruncatedCentralDirectory(t *testing.T) {
	files := map[string]string{
		"[Content_Types].xml": "<Types/>",
		"word/document.xml":   string(bytes.Repeat([]byte("<w:p>text</w:p>"), 200)),
		"word/styles.xml":     "<w:styles/>",
	}
	full := testZip(t, files)
	cut := full[:bytes.Index(full, []byte("PK\x01\x02"))+10]

	data, format := normalizeInput(cut, "docx")
	if format != "docx" {
		t.Fatalf("format = %q, want docx", format)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("salvaged zip does not open: %v", err)
	}
	if len(zr.File) != len(files) {
		t.Fatalf("salvaged %d entries, want %d", len(zr.File), len(files))
	}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if string(got) != files[f.Name] {
			t.Errorf("%s: content differs after salvage", f.Name)
		}
	}
}

func TestSalvageZipDropsIncompleteEntry(t *testing.T) {
	full := testZip(t, map[string]string{"a.xml": string(bytes.Repeat([]byte("a"), 500))})
	if _, err := salvageZip(full[:60]); err == nil {
		t.Fatal("expected an error when no entry is complete")
	}
}

func TestNormalizeInputLeavesValidZipUntouched(t *testing.T) {
	full := testZip(t, map[string]string{"word/document.xml": "<w/>"})
	if data, _ := normalizeInput(full, "docx"); !bytes.Equal(data, full) {
		t.Error("a valid zip must be passed through unchanged")
	}
}
