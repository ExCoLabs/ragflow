package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfficeOxideBootstrap(t *testing.T) {
	script, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, seed, cache, downloaded, platform string
		missingHeader, downloadFailure          bool
		missingArchiveHeader, corruptArchive    bool
		wantFailure                             bool
	}{
		{name: "matching_user", cache: "0.1.12"},
		{name: "matching_system", seed: "0.1.12"},
		{name: "newer_system", seed: "0.1.16", downloaded: "0.1.12"},
		{name: "older_user", cache: "0.1.7", downloaded: "0.1.12"},
		{name: "missing_library", downloaded: "0.1.12"},
		{name: "missing_header", cache: "0.1.12", missingHeader: true, downloaded: "0.1.12"},
		{name: "wrong_release", seed: "0.1.16", downloaded: "0.1.16", wantFailure: true},
		{name: "failed_download", seed: "0.1.16", downloadFailure: true, wantFailure: true},
		{name: "missing_archive_header", seed: "0.1.16", downloaded: "0.1.12", missingArchiveHeader: true, wantFailure: true},
		{name: "corrupt_archive", seed: "0.1.16", corruptArchive: true, wantFailure: true},
		{name: "unsupported_target", platform: "linux_mips", wantFailure: true},
		{name: "linux_arm64", seed: "0.1.16", downloaded: "0.1.12", platform: "linux_arm64"},
		{name: "darwin_amd64", seed: "0.1.16", downloaded: "0.1.12", platform: "darwin_amd64"},
		{name: "darwin_arm64", seed: "0.1.16", downloaded: "0.1.12", platform: "darwin_arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name string, data []byte, mode os.FileMode) {
				t.Helper()
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, mode); err != nil {
					t.Fatal(err)
				}
			}
			write("build.sh", script, 0o644)
			write("go.mod", []byte("module bootstrap\nrequire (\n\tgithub.com/yfedoseev/office_oxide/go v0.1.12\n)\n"), 0o644)
			legacy := "home/ragflow-native-libs/office_oxide"
			seed := "system/office_oxide"
			for prefix, version := range map[string]string{legacy: tc.cache, seed: tc.seed} {
				if version != "" {
					write(prefix+"/lib/liboffice_oxide.a", []byte(version+"\n"), 0o644)
					if !tc.missingHeader {
						write(prefix+"/include/office_oxide_c/office_oxide.h", []byte("header "+version+"\n"), 0o644)
					}
				}
			}
			var archive bytes.Buffer
			gz := gzip.NewWriter(&archive)
			tarball := tar.NewWriter(gz)
			for name, body := range map[string]string{
				"./lib/liboffice_oxide.a":                 tc.downloaded + "\n",
				"./include/office_oxide_c/office_oxide.h": "header " + tc.downloaded + "\n",
			} {
				if tc.missingArchiveHeader && strings.HasSuffix(name, ".h") {
					continue
				}
				if err := tarball.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
					t.Fatal(err)
				}
				if _, err := tarball.Write([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
			if err := tarball.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			payload := archive.Bytes()
			if tc.corruptArchive {
				payload = []byte("invalid archive")
			}
			write("release.tar.gz", payload, 0o644)
			write("bin/curl", []byte(`#!/bin/bash
while [ "$#" -gt 0 ]; do
 case "$1" in
  -o|--output) output="$2"; shift 2 ;;
  https://*) printf '%s\n' "$1" >> "$REQUESTS"; shift ;;
  *) shift ;;
 esac
done
if [ "$DOWNLOAD_FAILURE" = 1 ]; then printf partial > "$output"; exit 22; fi
cp "$RELEASE_ARCHIVE" "$output"
`), 0o755)
			write("bin/uv", []byte("#!/bin/bash\nprintf invoked > \"$UV_CALLED\"\nexit 97\n"), 0o755)
			platform := tc.platform
			if platform == "" {
				platform = "linux_amd64"
			}
			parts := strings.Split(platform, "_")
			failure := "0"
			if tc.downloadFailure {
				failure = "1"
			}
			cmd := exec.Command("bash", "-c", `source "$1"
SYSTEM_DEPS="$2"
detect_target_platform
check_office_oxide_deps
printf '%s' "$OFFICE_OXIDE_PREFIX" > "$3"
`, "bootstrap", filepath.Join(root, "build.sh"), filepath.Join(root, "system"), filepath.Join(root, "selected"))
			cmd.Env = append(os.Environ(), "HOME="+filepath.Join(root, "home"), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"), "RAGFLOW_TARGET_OS="+parts[0], "RAGFLOW_TARGET_ARCH="+parts[1], "RELEASE_ARCHIVE="+filepath.Join(root, "release.tar.gz"), "REQUESTS="+filepath.Join(root, "requests"), "UV_CALLED="+filepath.Join(root, "uv-called"), "DOWNLOAD_FAILURE="+failure)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantFailure {
				t.Fatalf("err=%v wantFailure=%t output=%s", err, tc.wantFailure, out)
			}
			if _, err := os.Stat(filepath.Join(root, "uv-called")); !os.IsNotExist(err) {
				t.Fatal("bootstrap must not invoke a Python downloader")
			}
			for prefix, version := range map[string]string{legacy: tc.cache, seed: tc.seed} {
				if version == "" {
					continue
				}
				got, err := os.ReadFile(filepath.Join(root, prefix, "lib/liboffice_oxide.a"))
				if err != nil || string(got) != version+"\n" {
					t.Fatalf("pre-existing library was changed: %s err=%v", prefix, err)
				}
			}
			staging, err := filepath.Glob(filepath.Join(root, legacy, "pinned", platform, ".office_oxide.*"))
			if err != nil || len(staging) != 0 {
				t.Fatalf("staging directories leaked: %v err=%v", staging, err)
			}
			if tc.wantFailure {
				if _, err := os.Stat(filepath.Join(root, legacy, "pinned", platform, "v0.1.12", "lib/liboffice_oxide.a")); !os.IsNotExist(err) {
					t.Fatal("failed download published a native library")
				}
				return
			}
			selected, err := os.ReadFile(filepath.Join(root, "selected"))
			if err != nil {
				t.Fatal(err)
			}
			library, err := os.ReadFile(filepath.Join(string(selected), "lib/liboffice_oxide.a"))
			if err != nil || string(library) != "0.1.12\n" {
				t.Fatalf("wrong selected library: %q err=%v", library, err)
			}
			header, err := os.ReadFile(filepath.Join(string(selected), "include/office_oxide_c/office_oxide.h"))
			if err != nil || string(header) != "header 0.1.12\n" {
				t.Fatalf("wrong selected header: %q err=%v", header, err)
			}
			requests, _ := os.ReadFile(filepath.Join(root, "requests"))
			if tc.downloaded == "" {
				if len(requests) != 0 {
					t.Fatal("matching pre-seed must not download")
				}
				return
			}
			assets := map[string]string{"linux_amd64": "native-linux-x86_64.tar.gz", "linux_arm64": "native-linux-aarch64.tar.gz", "darwin_amd64": "native-macos-x86_64.tar.gz", "darwin_arm64": "native-macos-aarch64.tar.gz"}
			want := "https://github.com/yfedoseev/office_oxide/releases/download/v0.1.12/" + assets[platform] + "\n"
			if string(requests) != want {
				t.Fatalf("request=%q want=%q", requests, want)
			}
			rerun := exec.Command(cmd.Path, cmd.Args[1:]...)
			rerun.Env = cmd.Env
			if out, err := rerun.CombinedOutput(); err != nil {
				t.Fatalf("cached rerun: err=%v output=%s", err, out)
			}
			again, _ := os.ReadFile(filepath.Join(root, "requests"))
			if string(again) != want {
				t.Fatal("pinned cache was downloaded again")
			}
		})
	}
}
