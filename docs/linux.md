# Linux Self-Update Guide

This document describes the currently supported end-to-end setup for Linux-based self-update flows.

## 1. Publish A Manifest

Your dist repository should publish a `latest.json` file and at least one downloadable archive artifact.

Example:

```json
{
  "version": "0.5.0",
  "source_commit": "abc1234",
  "build_time_utc": "2026-03-29T21:30:00Z",
  "artifacts": [
    {
      "name": "linux-amd64",
      "url": "linux.tar.gz"
    }
  ]
}
```

Recommendations:

- use stable artifact names such as `linux-amd64`, `linux-arm64`, `windows-amd64`
- keep URLs relative when the archive is served from the same dist base URL
- keep old fields only for transition periods

## 2. Package The Archive

The `tar.gz` payload can contain any internal structure. The package does not require hardcoded filenames anymore.

Example archive content:

```text
server
install.sh
templates/
static/
```

Or a more custom layout:

```text
payload/bin/server
payload/web/
payload/scripts/install
```

## 3. Define The Install Plan

Your application tells `go-selfupdate` how extracted source paths map to the real install location.

Example:

```go
entries := []selfupdate.InstallEntry{
    {Source: "server", Target: "server", Type: selfupdate.EntryTypeFile, Mode: 0o755},
    {Source: "install.sh", Target: "install.sh", Type: selfupdate.EntryTypeFile, Mode: 0o755},
    {Source: "templates", Target: "templates", Type: selfupdate.EntryTypeDir},
    {Source: "static", Target: "static", Type: selfupdate.EntryTypeDir},
}
```

Rules:

- `Source` is relative to the extracted archive root
- `Target` is relative to the real installation directory
- `file` entries are copied atomically
- `dir` entries replace the target directory with rollback on failure
- `Mode` is applied to the installed file or directory root when non-zero

## 4. Run The Update

```go
installDir, err := selfupdate.CheckSupport(baseURL)
if err != nil {
    return err
}

manifest, err := selfupdate.FetchManifest(ctx, baseURL)
if err != nil {
    return err
}

err = selfupdate.Apply(ctx, selfupdate.ApplyOptions{
    BaseURL:      baseURL,
    InstallDir:   installDir,
    Manifest:     manifest,
    ArtifactName: "linux-amd64",
    Entries:      entries,
})
if err != nil {
    return err
}
```

## 5. Restart The App

Restart is intentionally outside the package.

Typical patterns:

- send `SIGTERM` to self and let `systemd` restart it
- ask a supervisor process to restart the app
- exit cleanly after the update and let the process manager bring it back

## Current Scope

Implemented today:

- Linux environment validation
- `tar.gz` download and extraction
- configurable artifact selection
- configurable file and directory install mapping

Planned extension path:

- Windows-specific support checks
- Windows-friendly archive formats
- macOS support
- optional checksum/signature validation
