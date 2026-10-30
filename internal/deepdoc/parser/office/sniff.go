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
	"encoding/binary"
	"errors"
	"strings"
)

var (
	zipLocalHeaderMagic = []byte("PK\x03\x04")
	oleMagic            = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
)

var ooxmlFormatOf = map[string]string{"doc": "docx", "xls": "xlsx", "ppt": "pptx"}

// normalizeInput picks the office_oxide format from the file's magic bytes
// instead of trusting the extension, and rebuilds an OOXML zip whose central
// directory is cut off. File shares hold both: .docx saved as .doc, and
// copies truncated mid-write whose entries are all intact.
func normalizeInput(data []byte, format string) ([]byte, string) {
	format = strings.ToLower(format)
	switch {
	case bytes.HasPrefix(data, oleMagic):
		for legacy, ooxml := range ooxmlFormatOf {
			if format == ooxml {
				return data, legacy
			}
		}
		return data, format
	case bytes.HasPrefix(data, zipLocalHeaderMagic):
		if ooxml, ok := ooxmlFormatOf[format]; ok {
			format = ooxml
		}
		if _, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
			return data, format
		}
		if rebuilt, err := salvageZip(data); err == nil {
			return rebuilt, format
		}
	}
	return data, format
}

const (
	localHeaderLen     = 30
	flagDataDescriptor = 0x8
)

var errNothingSalvaged = errors.New("office: no complete zip entries to salvage")

// salvageZip rebuilds a zip from its local file headers, keeping every entry
// whose data is complete. It stops at the central directory or the first
// entry it cannot read whole.
func salvageZip(data []byte) ([]byte, error) {
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	entries := 0
	for off := 0; off+localHeaderLen <= len(data) && bytes.Equal(data[off:off+4], zipLocalHeaderMagic); {
		h := data[off : off+localHeaderLen]
		flags := binary.LittleEndian.Uint16(h[6:])
		compressed := int(binary.LittleEndian.Uint32(h[18:]))
		nameEnd := off + localHeaderLen + int(binary.LittleEndian.Uint16(h[26:]))
		dataStart := nameEnd + int(binary.LittleEndian.Uint16(h[28:]))
		// With a data descriptor the sizes live after the data, so the entry
		// length is unknown here.
		if flags&flagDataDescriptor != 0 || dataStart+compressed > len(data) {
			break
		}
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name:               string(data[off+localHeaderLen : nameEnd]),
			Method:             binary.LittleEndian.Uint16(h[8:]),
			CRC32:              binary.LittleEndian.Uint32(h[14:]),
			CompressedSize64:   uint64(compressed),
			UncompressedSize64: uint64(binary.LittleEndian.Uint32(h[22:])),
		})
		if err != nil {
			return nil, err
		}
		if _, err = w.Write(data[dataStart : dataStart+compressed]); err != nil {
			return nil, err
		}
		entries++
		off = dataStart + compressed
	}
	if entries == 0 {
		return nil, errNothingSalvaged
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
