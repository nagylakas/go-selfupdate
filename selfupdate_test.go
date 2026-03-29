package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want int
	}{
		{name: "equal", a: "0.4.9", b: "0.4.9", want: 0},
		{name: "less than", a: "0.4.8", b: "0.4.9", want: -1},
		{name: "greater than", a: "0.4.10", b: "0.4.9", want: 1},
		{name: "v prefix", a: "v1.2.0", b: "1.2", want: 0},
		{name: "missing parts default to zero", a: "1.2", b: "1.2.1", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompareVersions(tt.a, tt.b); got != tt.want {
				t.Fatalf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestFetchManifestArtifactList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest.json" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"version":"0.5.0",
			"source_commit":"abc123",
			"build_time_utc":"2026-03-29T20:00:00Z",
			"artifacts":[
				{"name":"linux-amd64","url":"linux.tar.gz"},
				{"name":"windows-amd64","url":"windows.zip"}
			]
		}`))
	}))
	defer server.Close()

	manifest, err := FetchManifest(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("FetchManifest returned error: %v", err)
	}
	if manifest.Version != "0.5.0" {
		t.Fatalf("manifest.Version = %q, want %q", manifest.Version, "0.5.0")
	}
	if len(manifest.Artifacts) != 2 {
		t.Fatalf("len(manifest.Artifacts) = %d, want 2", len(manifest.Artifacts))
	}
	if manifest.Artifacts[0].Name != "linux-amd64" {
		t.Fatalf("manifest.Artifacts[0].Name = %q, want %q", manifest.Artifacts[0].Name, "linux-amd64")
	}
}

func TestFetchManifestLegacyLinuxArchive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"version":"0.4.9",
			"linux_archive":"linux.tar.gz"
		}`))
	}))
	defer server.Close()

	manifest, err := FetchManifest(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("FetchManifest returned error: %v", err)
	}
	if len(manifest.Artifacts) != 1 {
		t.Fatalf("len(manifest.Artifacts) = %d, want 1", len(manifest.Artifacts))
	}
	if manifest.Artifacts[0].Name != "linux" {
		t.Fatalf("manifest.Artifacts[0].Name = %q, want %q", manifest.Artifacts[0].Name, "linux")
	}
}

func TestApplyUsesArtifactAndInstallEntries(t *testing.T) {
	archiveData := mustCreateTarGz(t, map[string]string{
		"payload/bin/server":        "server-binary",
		"payload/scripts/install":   "#!/bin/sh\necho install\n",
		"payload/web/index.html":    "<h1>new</h1>",
		"payload/web/assets/app.js": "console.log('ok')",
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pkg/linux.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archiveData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(installDir, "www"), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "www", "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	manifest := &Manifest{
		Version: "0.5.0",
		Artifacts: []Artifact{
			{Name: "linux-amd64", URL: server.URL + "/pkg/linux.tar.gz"},
		},
	}

	var steps []ProgressStep
	err := Apply(context.Background(), ApplyOptions{
		InstallDir:   installDir,
		Manifest:     manifest,
		ArtifactName: "linux-amd64",
		Entries: []InstallEntry{
			{Source: "payload/bin/server", Target: "bin/server", Type: EntryTypeFile, Mode: 0o755},
			{Source: "payload/scripts/install", Target: "scripts/install.sh", Type: EntryTypeFile, Mode: 0o755},
			{Source: "payload/web", Target: "www", Type: EntryTypeDir},
		},
		OnProgress: func(step ProgressStep) {
			steps = append(steps, step)
		},
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(installDir, "bin", "server")); err != nil {
		t.Fatalf("server was not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "scripts", "install.sh")); err != nil {
		t.Fatalf("install script was not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "www", "index.html")); err != nil {
		t.Fatalf("www directory was not replaced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "www", "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old file should have been removed during directory replace")
	}
	if len(steps) != 3 || steps[0] != StepDownloading || steps[1] != StepExtracting || steps[2] != StepInstalling {
		t.Fatalf("unexpected progress steps: %#v", steps)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(installDir, "bin", "server"))
		if err != nil {
			t.Fatalf("Stat failed: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("server mode = %o, want %o", info.Mode().Perm(), 0o755)
		}
	}
}

func TestResolveArtifactURL(t *testing.T) {
	tests := []struct {
		name      string
		baseURL   string
		artifact  string
		wantValue string
	}{
		{
			name:      "relative path",
			baseURL:   "https://example.com/dist/",
			artifact:  "linux.tar.gz",
			wantValue: "https://example.com/dist/linux.tar.gz",
		},
		{
			name:      "absolute url",
			baseURL:   "https://example.com/dist",
			artifact:  "https://cdn.example.com/linux.tar.gz",
			wantValue: "https://cdn.example.com/linux.tar.gz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveArtifactURL(tt.baseURL, tt.artifact)
			if err != nil {
				t.Fatalf("resolveArtifactURL(%q, %q) returned error: %v", tt.baseURL, tt.artifact, err)
			}
			if got != tt.wantValue {
				t.Fatalf("resolveArtifactURL(%q, %q) = %q, want %q", tt.baseURL, tt.artifact, got, tt.wantValue)
			}
		})
	}
}

func mustCreateTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	archiveFile, err := os.CreateTemp(t.TempDir(), "payload-*.tar.gz")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer archiveFile.Close()

	gzw := gzip.NewWriter(archiveFile)
	tw := tar.NewWriter(gzw)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if strings.HasSuffix(name, "server") || strings.Contains(name, "/install") {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader failed: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close failed: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}

	data, err := os.ReadFile(archiveFile.Name())
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	return data
}
