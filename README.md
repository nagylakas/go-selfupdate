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
      "url": "linux.tar.gz"
    }
  ]
}
```

Backward compatibility:

- `FetchManifest` still accepts the legacy `linux_archive` field
- when only the legacy field exists, it is exposed as a single artifact named `linux`

## Typical Flow

```go
installDir, err := selfupdate.CheckSupport(baseURL)
if err != nil {
    return err
}

manifest, err := selfupdate.FetchManifest(ctx, baseURL)
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
