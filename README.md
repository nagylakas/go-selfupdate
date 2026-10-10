# go-selfupdate

Reusable self-update helper for Go applications that distribute packaged builds and want to install them in place.

The package is Linux-first today, but the manifest and install-plan model are platform-neutral:

- the manifest publishes an artifact list instead of a single hardcoded archive
- the caller chooses which artifact to install
- the caller provides an explicit install plan with file and directory mappings
- restart logic stays outside the package, in the application

Current runtime support:

- automatic support check is implemented for Linux
- archive extraction currently expects `tar.gz`
- install operations support file copy and full directory replacement

For a step-by-step Linux integration guide, see [docs/linux.md](docs/linux.md).

## Concepts

`Manifest`

- remote metadata from `latest.json`
- contains app version info and downloadable artifacts

`Artifact`

- a named downloadable package such as `linux-amd64`

`InstallEntry`

- a single mapping from extracted source path to target install path
- type can be `file` or `dir`
- `Mode` controls the final chmod for that installed target

`ApplyOptions`

- selects the artifact to install
- defines the install root
- defines the install entries to apply after extraction

## Manifest Format

Recommended format:

```json
{
  "version": "0.5.0",
  "source_commit": "abc1234",
  "build_time_utc": "2026-03-29T21:30:00Z",
  "artifacts": [
    {
      "name": "linux-amd64",
      "url": "linux.tar.gz",
      "sha256": "<lowercase hex sha256 of linux.tar.gz>"
    }
  ],
  "signature": "<base64 ed25519 signature written by SignManifest>"
}
```

`sha256` and `signature` are optional for `FetchManifest`, but required when the
caller passes trusted public keys (see Integrity below).

Backward compatibility:

- `FetchManifest` still accepts the legacy `linux_archive` field
- when only the legacy field exists, it is exposed as a single artifact named `linux`

## Integrity

Unattended updates must only install packages you built. Since v0.2.0:

- `Artifact.SHA256`: when present, `Apply` verifies the downloaded archive against it
  **before extraction**; `ApplyOptions.RequireChecksum` makes it mandatory.
- `ApplyOptions.MaxDownloadBytes` caps the download (default `DefaultMaxDownloadBytes`,
  512 MiB); `FetchOptions.MaxBytes` caps `latest.json` (default 1 MiB).
- Signed manifests: the release tooling calls `SignManifest(manifest, privateKey)`
  (ed25519, key kept outside the repository). Clients compile in the public key(s)
  and call `FetchManifestWithOptions(ctx, FetchOptions{BaseURL: ..., PublicKeys: keys})`;
  an unsigned, tampered or foreign-key manifest is rejected, and every artifact must
  carry a checksum. Passing the same `PublicKeys` to `Apply` re-verifies the manifest
  and implies `RequireChecksum`.
- The signature covers a canonical JSON encoding of `version`, `source_commit`,
  `build_time_utc` and `artifacts` (name, url, sha256), prefixed with
  `go-selfupdate/manifest/v1
` — not the raw file bytes, so git line-ending
  normalization of `latest.json` does not invalidate it. The legacy `linux_archive`
  field is ignored for signed manifests.
- Every integrity failure wraps `ErrIntegrity` (`ErrUnsigned`, `ErrBadSignature`,
  `ErrInvalidManifest`, `ErrChecksumMissing`, `ErrChecksumMismatch`, `ErrTooLarge`),
  so callers can tell an attack or broken release from a network error with
  `errors.Is(err, selfupdate.ErrIntegrity)`.
- `HTTPClient` on both option structs replaces `http.DefaultClient`.

## Typical Flow

```go
installDir, err := selfupdate.CheckSupport(baseURL)
if err != nil {
    return err
}

manifest, err := selfupdate.FetchManifestWithOptions(ctx, selfupdate.FetchOptions{
    BaseURL:    baseURL,
    PublicKeys: trustedKeys, // compiled-in ed25519 public keys
})
if err != nil {
    return err
}

if selfupdate.CompareVersions(currentVersion, manifest.Version) >= 0 {
    return nil
}

err = selfupdate.Apply(ctx, selfupdate.ApplyOptions{
    BaseURL:      baseURL,
    InstallDir:   installDir,
    Manifest:     manifest,
    ArtifactName: "linux-amd64",
    PublicKeys:   trustedKeys,
    Entries: []selfupdate.InstallEntry{
        {Source: "server", Target: "server", Type: selfupdate.EntryTypeFile, Mode: 0o755},
        {Source: "install.sh", Target: "install.sh", Type: selfupdate.EntryTypeFile, Mode: 0o755},
        {Source: "templates", Target: "templates", Type: selfupdate.EntryTypeDir},
        {Source: "static", Target: "static", Type: selfupdate.EntryTypeDir},
    },
})
if err != nil {
    return err
}

// restart is the application's responsibility
```

## Design Notes

- The package intentionally does not decide what to install.
- The package intentionally does not decide how the process restarts.
- The package validates relative paths so archives and install plans cannot escape the intended root.
- The current `CheckSupport` implementation is Linux-specific, but the manifest and install-entry API are designed so Windows and macOS support can be added later without changing the public model again.
