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

package file

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"ragflow/internal/storage"
)

func TestAttachmentRef_RoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ref := AttachmentRef{Owner: "user-1", ID: "file-id", Name: "rozpočet.xlsx", MimeType: "application/vnd.ms-excel", Expires: now.Add(time.Hour).Unix()}
	encoded, err := ref.Encode("secret")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := DecodeAttachmentRef(encoded, "secret", now)
	if err != nil {
		t.Fatalf("DecodeAttachmentRef: %v", err)
	}
	if *got != ref {
		t.Fatalf("decoded = %+v, want %+v", *got, ref)
	}
}

func TestAttachmentRef_Rejects(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	valid := AttachmentRef{Owner: "user-1", ID: "file-id", Expires: now.Add(time.Hour).Unix()}
	encoded, err := valid.Encode("secret")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	expired := AttachmentRef{Owner: "user-1", ID: "file-id", Expires: now.Add(-time.Second).Unix()}
	expiredEncoded, _ := expired.Encode("secret")
	forged := AttachmentRef{Owner: "user-2", ID: "file-id", Expires: now.Add(time.Hour).Unix()}
	forgedEncoded, _ := forged.Encode("secret")
	payload, _, _ := strings.Cut(forgedEncoded, ".")
	_, sig, _ := strings.Cut(encoded, ".")

	cases := map[string]struct{ value, secret string }{
		"wrong secret":      {encoded, "other"},
		"empty secret":      {encoded, ""},
		"expired":           {expiredEncoded, "secret"},
		"swapped payload":   {payload + "." + sig, "secret"},
		"missing signature": {payload, "secret"},
		"not base64":        {"!!!.???", "secret"},
		"empty":             {"", "secret"},
	}
	for name, tc := range cases {
		if _, err := DecodeAttachmentRef(tc.value, tc.secret, now); err == nil {
			t.Errorf("%s: decoded, want error", name)
		}
	}
}

func TestFileService_ParseAgentUploads_AddsAttachmentRef(t *testing.T) {
	ctx := t.Context()
	memory := storage.NewMemoryStorage()
	if err := memory.Put(ctx, "user-1-downloads", "text-id", []byte("uploaded text")); err != nil {
		t.Fatalf("put text: %v", err)
	}
	if err := memory.Put(ctx, "user-1-downloads", "image-id", []byte("png")); err != nil {
		t.Fatalf("put image: %v", err)
	}
	factory := storage.GetStorageFactory()
	originalStorage := factory.GetStorage()
	factory.SetStorage(memory)
	t.Cleanup(func() { factory.SetStorage(originalStorage) })

	contents, err := testFileService().ParseAgentUploads(ctx, "user-1", []map[string]interface{}{
		{"id": "text-id", "name": "notes.txt", "mime_type": "text/plain; charset=utf-8", "created_by": "user-1"},
		{"id": "image-id", "name": "photo.png", "mime_type": "image/png", "created_by": "user-1"},
	}, "Plain Text", "secret")
	if err != nil {
		t.Fatalf("ParseAgentUploads: %v", err)
	}
	m := regexp.MustCompile(`Attachment ref: (\S+)\n`).FindStringSubmatch(contents[0])
	if m == nil {
		t.Fatalf("no attachment ref in %q", contents[0])
	}
	ref, err := DecodeAttachmentRef(m[1], "secret", time.Now())
	if err != nil {
		t.Fatalf("DecodeAttachmentRef: %v", err)
	}
	if ref.Owner != "user-1" || ref.ID != "text-id" || ref.Name != "notes.txt" || ref.MimeType != "text/plain" {
		t.Fatalf("ref = %+v", *ref)
	}
	if !strings.Contains(contents[0], "uploaded text") {
		t.Fatalf("content lost: %q", contents[0])
	}
	if contents[1] != "data:image/png;base64,cG5n" {
		t.Fatalf("image content changed: %q", contents[1])
	}
}
