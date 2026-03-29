package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Manifest describes the latest distributable build available for self-update.
type Manifest struct {
	Version      string     `json:"version"`
	SourceCommit string     `json:"source_commit"`
	BuildTimeUTC string     `json:"build_time_utc"`
	Artifacts    []Artifact `json:"artifacts"`
}

// Artifact identifies a downloadable update package.
type Artifact struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ProgressStep identifies a coarse-grained step in the update process.
type ProgressStep string

const (
	StepDownloading ProgressStep = "downloading"
	StepExtracting  ProgressStep = "extracting"
	StepInstalling  ProgressStep = "installing"
)

// EntryType describes how an extracted payload entry should be installed.
type EntryType string

const (
	EntryTypeFile EntryType = "file"
	EntryTypeDir  EntryType = "dir"
)

// InstallEntry maps a source path from the extracted archive to a target path in InstallDir.
type InstallEntry struct {
	Source string
	Target string
	Type   EntryType
	Mode   os.FileMode
}

// ApplyOptions configures the update installation process.
type ApplyOptions struct {
	BaseURL      string
	InstallDir   string
	Manifest     *Manifest
	ArtifactName string
	Entries      []InstallEntry
	OnProgress   func(step ProgressStep)
}

// manifestPayload is the raw JSON structure of the update manifest as served by the remote endpoint.
// It extends the public Manifest type with legacy fields for backwards compatibility.
type manifestPayload struct {
	Version         string     `json:"version"`
	SourceCommit    string     `json:"source_commit"`
	BuildTimeUTC    string     `json:"build_time_utc"`
	Artifacts       []Artifact `json:"artifacts"`
	LegacyLinuxFile string     `json:"linux_archive"`
}

// FetchManifest downloads and validates the update manifest from the given base URL.
func FetchManifest(ctx context.Context, baseURL string) (*Manifest, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("update base URL is not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/latest.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("failed to fetch update manifest")
	}

	var payload manifestPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Version == "" {
		return nil, errors.New("update manifest is missing a version")
	}

	artifacts := payload.Artifacts
	if len(artifacts) == 0 && strings.TrimSpace(payload.LegacyLinuxFile) != "" {
		artifacts = []Artifact{{
			Name: "linux",
			URL:  payload.LegacyLinuxFile,
		}}
	}
	if len(artifacts) == 0 {
		return nil, errors.New("update manifest is missing artifacts")
	}
	if err := validateArtifacts(artifacts); err != nil {
		return nil, err
	}

	return &Manifest{
		Version:      payload.Version,
		SourceCommit: payload.SourceCommit,
		BuildTimeUTC: payload.BuildTimeUTC,
		Artifacts:    artifacts,
	}, nil
}

// CheckSupport verifies that self-update can run in the current environment.
// It returns the resolved install directory when the environment is supported.
func CheckSupport(baseURL string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("self-update is only supported on Linux")
	}
	if strings.TrimSpace(baseURL) == "" {
		return "", errors.New("update source is not configured")
	}

	installDir, err := CurrentInstallDir()
	if err != nil {
		return "", errors.New("install directory is unavailable")
	}

	testDir, err := os.MkdirTemp(installDir, ".update-check-*")
	if err != nil {
		return "", errors.New("install directory is not writable by the running process")
	}
	_ = os.RemoveAll(testDir)
	return installDir, nil
}

// CurrentInstallDir resolves the directory of the currently running executable.
func CurrentInstallDir() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exePath), nil
}

// Apply downloads, validates and installs the selected update artifact into InstallDir.
func Apply(ctx context.Context, opts ApplyOptions) error {
	if opts.Manifest == nil {
		return errors.New("update manifest is required")
	}
	if strings.TrimSpace(opts.InstallDir) == "" {
		return errors.New("install directory is required")
	}
	if err := validateInstallEntries(opts.Entries); err != nil {
		return errors.New("invalid install entries: " + err.Error())
	}

	artifact, err := selectArtifact(opts.Manifest, opts.ArtifactName)
	if err != nil {
		return errors.New("select artifact: " + err.Error())
	}

	archiveURL, err := resolveArtifactURL(strings.TrimRight(opts.BaseURL, "/"), artifact.URL)
	if err != nil {
		return errors.New("resolve artifact URL: " + err.Error())
	}

	tmpDir, err := os.MkdirTemp("", "selfupdate-*")
	if err != nil {
		return errors.New("create temp directory: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, "artifact.tar.gz")
	notifyProgress(opts.OnProgress, StepDownloading)
	if err := downloadFile(ctx, archiveURL, archivePath); err != nil {
		return errors.New("download update: " + err.Error())
	}

	extractDir := filepath.Join(tmpDir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return errors.New("prepare extraction directory: " + err.Error())
	}

	notifyProgress(opts.OnProgress, StepExtracting)
	if err := extractTarGz(archivePath, extractDir); err != nil {
		return errors.New("extract update: " + err.Error())
	}
	if err := validatePayload(extractDir, opts.Entries); err != nil {
		return errors.New("invalid update package: " + err.Error())
	}

	notifyProgress(opts.OnProgress, StepInstalling)
	if err := installPayload(extractDir, opts.InstallDir, opts.Entries); err != nil {
		return errors.New("install update: " + err.Error())
	}

	return nil
}

// CompareVersions compares semantic-ish dotted numeric versions like 0.4.8 and 0.4.10.
func CompareVersions(a, b string) int {
	partsA := strings.Split(strings.TrimPrefix(strings.TrimSpace(a), "v"), ".")
	partsB := strings.Split(strings.TrimPrefix(strings.TrimSpace(b), "v"), ".")
	maxLen := len(partsA)
	if len(partsB) > maxLen {
		maxLen = len(partsB)
	}
	for i := 0; i < maxLen; i++ {
		va := 0
		if i < len(partsA) {
			va, _ = strconv.Atoi(partsA[i])
		}
		vb := 0
		if i < len(partsB) {
			vb, _ = strconv.Atoi(partsB[i])
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}

// validateArtifacts checks that every artifact in the list has a non-empty name and URL,
// and that no two artifacts share the same name.
func validateArtifacts(artifacts []Artifact) error {
	seen := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		name := strings.TrimSpace(artifact.Name)
		url := strings.TrimSpace(artifact.URL)
		if name == "" {
			return errors.New("update manifest contains an artifact with an empty name")
		}
		if url == "" {
			return errors.New("update manifest contains an artifact with an empty URL")
		}
		if seen[name] {
			return errors.New("update manifest contains duplicate artifact names")
		}
		seen[name] = true
	}
	return nil
}

// validateInstallEntries verifies that the caller-supplied install entries are well-formed:
// each entry must have a valid relative source and target path, a recognised EntryType,
// and no two entries may share the same normalised target path.
func validateInstallEntries(entries []InstallEntry) error {
	if len(entries) == 0 {
		return errors.New("at least one install entry is required")
	}

	targets := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if _, err := normalizeRelativePath(entry.Source); err != nil {
			return errors.New("invalid source path: " + err.Error())
		}
		target, err := normalizeRelativePath(entry.Target)
		if err != nil {
			return errors.New("invalid target path: " + err.Error())
		}
		switch entry.Type {
		case EntryTypeFile, EntryTypeDir:
		default:
			return errors.New("unsupported entry type: " + string(entry.Type))
		}
		if targets[target] {
			return errors.New("duplicate install target path")
		}
		targets[target] = true
	}
	return nil
}

// selectArtifact returns the artifact matching name from manifest.
// If name is empty and the manifest contains exactly one artifact, that artifact is returned.
// An error is returned when no match is found or the selection is ambiguous.
func selectArtifact(manifest *Manifest, name string) (*Artifact, error) {
	if manifest == nil {
		return nil, errors.New("manifest is required")
	}
	if len(manifest.Artifacts) == 0 {
		return nil, errors.New("manifest contains no artifacts")
	}
	if strings.TrimSpace(name) == "" {
		if len(manifest.Artifacts) == 1 {
			return &manifest.Artifacts[0], nil
		}
		return nil, errors.New("artifact name is required when multiple artifacts are available")
	}
	for i := range manifest.Artifacts {
		if manifest.Artifacts[i].Name == name {
			return &manifest.Artifacts[i], nil
		}
	}
	return nil, errors.New("artifact not found: " + name)
}

// notifyProgress calls fn with step if fn is non-nil. It is a no-op when the caller
// did not provide a progress callback.
func notifyProgress(fn func(step ProgressStep), step ProgressStep) {
	if fn != nil {
		fn(step)
	}
}

// resolveArtifactURL returns the absolute URL for an artifact.
// If archiveURL is already an absolute http:// or https:// URL it is returned as-is.
// Otherwise it is treated as a path relative to baseURL.
// URLs with any other scheme (e.g. file://, ftp://) are rejected with an error.
func resolveArtifactURL(baseURL, archiveURL string) (string, error) {
	archiveURL = strings.TrimSpace(archiveURL)
	if strings.HasPrefix(archiveURL, "http://") || strings.HasPrefix(archiveURL, "https://") {
		return archiveURL, nil
	}
	if strings.Contains(archiveURL, "://") {
		return "", errors.New("artifact URL uses an unsupported scheme")
	}
	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(archiveURL, "/"), nil
}

// normalizeRelativePath cleans and validates a relative file-system path.
// It rejects empty paths, absolute paths, and any path whose first component is "..".
// The returned path uses the host OS separator and has no leading or trailing slashes.
func normalizeRelativePath(value string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(value)))
	if cleaned == "." || cleaned == "" {
		return "", errors.New("path must not be empty")
	}
	if filepath.IsAbs(cleaned) {
		return "", errors.New("path must be relative")
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("path must not escape the install root")
	}
	return cleaned, nil
}

// safeJoin joins root and value into an absolute path and verifies that the result
// stays within root. It prevents directory-traversal attacks by rejecting any joined
// path whose relative form would escape the root directory.
func safeJoin(root, value string) (string, error) {
	normalized, err := normalizeRelativePath(value)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	target := filepath.Join(root, normalized)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path must not escape the target root")
	}
	return target, nil
}

// downloadFile performs an HTTP GET for url and writes the response body to dest.
// The request honours ctx for cancellation. An error is returned for any non-200 status.
func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("unexpected HTTP status: " + strconv.Itoa(resp.StatusCode))
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// extractTarGz decompresses and extracts the gzip-compressed tar archive at archivePath
// into destDir. Only regular files and directories are accepted; symlinks and hardlinks
// are rejected to prevent tar-slip attacks. All entry names are validated with safeJoin
// to ensure they cannot escape destDir.
func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		name := filepath.Clean(filepath.FromSlash(strings.TrimSpace(hdr.Name)))
		if name == "." {
			continue
		}
		target, err := safeJoin(destDir, name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return errors.New("symlinks and hardlinks are not supported in update archives")
		}
	}
}

// validatePayload checks that every install entry is present in the extracted archive
// directory rooted at root and that its type matches the declared EntryType.
// os.Lstat is used deliberately so that symlinks are never silently accepted.
func validatePayload(root string, entries []InstallEntry) error {
	for _, entry := range entries {
		sourcePath, err := safeJoin(root, entry.Source)
		if err != nil {
			return err
		}
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return errors.New(entry.Source + " is missing")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New(entry.Source + " is a symlink (not supported)")
		}
		switch entry.Type {
		case EntryTypeFile:
			if info.IsDir() {
				return errors.New(entry.Source + " is a directory, expected a file")
			}
		case EntryTypeDir:
			if !info.IsDir() {
				return errors.New(entry.Source + " is not a directory")
			}
		}
	}
	return nil
}

// installPayload copies each entry from srcRoot into dstRoot according to its EntryType.
// Files are written atomically; directories are replaced with a backup-and-rename strategy.
func installPayload(srcRoot, dstRoot string, entries []InstallEntry) error {
	for _, entry := range entries {
		src, err := safeJoin(srcRoot, entry.Source)
		if err != nil {
			return err
		}
		dst, err := safeJoin(dstRoot, entry.Target)
		if err != nil {
			return err
		}

		switch entry.Type {
		case EntryTypeFile:
			if err := copyFileAtomic(src, dst, entry.Mode); err != nil {
				return err
			}
		case EntryTypeDir:
			if err := replaceDir(src, dst, entry.Mode); err != nil {
				return err
			}
		default:
			return errors.New("unsupported entry type: " + string(entry.Type))
		}
	}
	return nil
}

// copyFileAtomic copies src to dst using a write-to-temp-then-rename strategy so that
// dst is never left in a partially written state. If chmodMode is non-zero it is applied
// to the destination file after the rename. Symlinks in src are rejected.
func copyFileAtomic(src, dst string, chmodMode os.FileMode) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("symlinks are not supported: " + src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	if err := out.Chmod(info.Mode()); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if chmodMode != 0 {
		if err := os.Chmod(tmp, chmodMode); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// replaceDir atomically replaces the directory at dst with the contents of src.
// It first copies src to a temporary staging directory (dst+".new"), then renames
// the existing dst to a backup (dst+".old") before promoting the staged copy.
// On failure after the backup rename, it attempts to restore the backup.
// Symlinks at the dst location are rejected to prevent attacks via pre-placed links.
func replaceDir(src, dst string, chmodMode os.FileMode) error {
	tmp := dst + ".new"
	backup := dst + ".old"

	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	if err := copyDir(src, tmp); err != nil {
		return err
	}

	if dstInfo, err := os.Lstat(dst); err == nil {
		if dstInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("install target is a symlink (not supported): " + dst)
		}
		if err := os.Rename(dst, backup); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.Rename(tmp, dst); err != nil {
		if _, statErr := os.Stat(backup); statErr == nil {
			_ = os.Rename(backup, dst)
		}
		return err
	}
	if chmodMode != 0 {
		if err := os.Chmod(dst, chmodMode); err != nil {
			return err
		}
	}
	return os.RemoveAll(backup)
}

// copyDir recursively copies the directory tree rooted at src into dst,
// preserving file modes. Symlinks encountered during the walk are rejected
// to prevent traversal of attacker-controlled link targets.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlinks are not supported: " + path)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, info.Mode())
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return copyFileAtomic(path, target, info.Mode())
	})
}
