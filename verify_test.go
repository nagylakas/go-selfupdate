package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signedTestManifest(t *testing.T, priv ed25519.PrivateKey, archive []byte) *Manifest {
	t.Helper()
	m := &Manifest{
		Version:      "0.6.0",
		SourceCommit: "abc1234",
		BuildTimeUTC: "2026-10-10T12:00:00Z",
		Artifacts:    []Artifact{{Name: "linux-amd64", URL: "linux.tar.gz", SHA256: SHA256Hex(archive)}},
	}
	if err := SignManifest(m, priv); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// updateServer serves latest.json and linux.tar.gz.
func updateServer(t *testing.T, manifest, archive []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			_, _ = w.Write(manifest)
		case "/linux.tar.gz":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

var testEntries = []InstallEntry{{Source: "server", Target: "server", Type: EntryTypeFile, Mode: 0o755}}

func TestSignedManifestAndArtifactAreInstalled(t *testing.T) {
	pub, priv := newTestKey(t)
	archive := mustCreateTarGz(t, map[string]string{"server": "new-binary"})
	server := updateServer(t, mustJSON(t, signedTestManifest(t, priv, archive)), archive)
	keys := []ed25519.PublicKey{pub}

	manifest, err := FetchManifestWithOptions(context.Background(), FetchOptions{BaseURL: server.URL, PublicKeys: keys})
	if err != nil {
		t.Fatalf("signed manifest rejected: %v", err)
	}
	installDir := t.TempDir()
	if err := Apply(context.Background(), ApplyOptions{
		BaseURL: server.URL, InstallDir: installDir, Manifest: manifest,
		ArtifactName: "linux-amd64", Entries: testEntries, PublicKeys: keys,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(installDir, "server")); err != nil || string(data) != "new-binary" {
		t.Fatalf("server not installed: %q, %v", data, err)
	}
}

func TestSignatureIsLineEndingIndependent(t *testing.T) {
	pub, priv := newTestKey(t)
	data := strings.ReplaceAll(string(mustJSON(t, signedTestManifest(t, priv, []byte("x")))), "\n", "\r\n")
	if _, err := ParseManifest(append([]byte{0xef, 0xbb, 0xbf}, data...), []ed25519.PublicKey{pub}); err != nil {
		t.Fatalf("CRLF manifest rejected: %v", err)
	}
}

func TestUnsignedOrTamperedManifestIsRejected(t *testing.T) {
	pub, priv := newTestKey(t)
	_, otherPriv := newTestKey(t)
	keys := []ed25519.PublicKey{pub}
	archive := []byte("archive")

	unsigned := signedTestManifest(t, priv, archive)
	unsigned.Signature = ""
	legacy := []byte(`{"version":"9.0.0","linux_archive":"linux.tar.gz"}`)
	cases := map[string]struct {
		data []byte
		want error
	}{
		"unsigned":      {mustJSON(t, unsigned), ErrUnsigned},
		"legacy only":   {legacy, ErrInvalidManifest},
		"untrusted key": {mustJSON(t, signedTestManifest(t, otherPriv, archive)), ErrBadSignature},
	}
	for field, tamper := range map[string]func(*Manifest){
		"version": func(m *Manifest) { m.Version = "9.9.9" },
		"url":     func(m *Manifest) { m.Artifacts[0].URL = "https://evil.example/x.tar.gz" },
		"sha256":  func(m *Manifest) { m.Artifacts[0].SHA256 = SHA256Hex([]byte("evil")) },
		"garbage": func(m *Manifest) { m.Signature = "@@@" },
	} {
		m := signedTestManifest(t, priv, archive)
		tamper(m)
		cases["tampered "+field] = struct {
			data []byte
			want error
		}{mustJSON(t, m), ErrBadSignature}
	}

	for name, tc := range cases {
		server := updateServer(t, tc.data, archive)
		_, err := FetchManifestWithOptions(context.Background(), FetchOptions{BaseURL: server.URL, PublicKeys: keys})
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrIntegrity) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}

	// Apply re-verifies a manifest handed to it directly.
	m := signedTestManifest(t, priv, archive)
	m.Version = "9.9.9"
	err := Apply(context.Background(), ApplyOptions{InstallDir: t.TempDir(), Manifest: m, ArtifactName: "linux-amd64", Entries: testEntries, PublicKeys: keys})
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Apply with tampered manifest: err = %v", err)
	}
}

func TestChecksumMismatchIsRejectedBeforeExtraction(t *testing.T) {
	good := mustCreateTarGz(t, map[string]string{"server": "good"})
	evil := mustCreateTarGz(t, map[string]string{"server": "evil"})
	server := updateServer(t, nil, evil)
	installDir := t.TempDir()

	manifest := &Manifest{Version: "1.0.0", Artifacts: []Artifact{{Name: "linux-amd64", URL: "linux.tar.gz", SHA256: SHA256Hex(good)}}}
	err := Apply(context.Background(), ApplyOptions{BaseURL: server.URL, InstallDir: installDir, Manifest: manifest, ArtifactName: "linux-amd64", Entries: testEntries})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(installDir, "server")); !os.IsNotExist(statErr) {
		t.Fatal("tampered archive was installed")
	}
}

func TestMissingChecksumAndOversizedDownloadAreRejected(t *testing.T) {
	archive := mustCreateTarGz(t, map[string]string{"server": "binary"})
	server := updateServer(t, nil, archive)

	unchecked := &Manifest{Version: "1.0.0", Artifacts: []Artifact{{Name: "linux-amd64", URL: "linux.tar.gz"}}}
	err := Apply(context.Background(), ApplyOptions{BaseURL: server.URL, InstallDir: t.TempDir(), Manifest: unchecked, ArtifactName: "linux-amd64", Entries: testEntries, RequireChecksum: true})
	if !errors.Is(err, ErrChecksumMissing) {
		t.Fatalf("missing checksum: err = %v", err)
	}

	checked := &Manifest{Version: "1.0.0", Artifacts: []Artifact{{Name: "linux-amd64", URL: "linux.tar.gz", SHA256: SHA256Hex(archive)}}}
	err = Apply(context.Background(), ApplyOptions{BaseURL: server.URL, InstallDir: t.TempDir(), Manifest: checked, ArtifactName: "linux-amd64", Entries: testEntries, MaxDownloadBytes: int64(len(archive) - 1)})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized download: err = %v", err)
	}

	big := updateServer(t, make([]byte, 64), nil)
	if _, err := FetchManifestWithOptions(context.Background(), FetchOptions{BaseURL: big.URL, MaxBytes: 32}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized manifest: err = %v", err)
	}
}

func TestSignManifestRequiresChecksums(t *testing.T) {
	_, priv := newTestKey(t)
	m := &Manifest{Version: "1.0.0", Artifacts: []Artifact{{Name: "a", URL: "a"}}}
	if err := SignManifest(m, priv); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("err = %v", err)
	}
	m.Artifacts[0].SHA256 = strings.ToUpper(SHA256Hex(nil))
	if err := SignManifest(m, priv); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("uppercase digest: err = %v", err)
	}
}
