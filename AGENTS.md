# Go Agent Instructions

## Scope and References

- This is the independent Go repository `github.com/mingcheng/stupidict`.
  Apply Go tooling here; the upstream .NET/Avalonia implementation is separate.
- Preserve the public module path `github.com/mingcheng/stupidict`, package
  name `dictionary`, and application name `stupidict`.
- Before changing lookup or matching, read [Behavior preserved](README.md#behavior-preserved)
  and its linked C# references. The .NET app is a semantic reference, not a
  runtime or test dependency.
- Keep the root library read-only and presentation-independent. Route frontend,
  persistence, download, and playback changes through the owners in
  [Architecture and scope](README.md#architecture-and-scope).
- Consult [Terminal client](README.md#terminal-client) for profile isolation,
  [Initialization and data updates](README.md#initialization-and-data-updates)
  for replacement/retry contracts, and [Interaction](README.md#interaction)
  before changing asynchronous UI behavior or navigation.

## Local Development

- Use Go 1.27.2 or newer. Commands here and in the README run from this directory,
  which contains `go.mod`; do not add `-C go` when already here.
- Data downloads still use the upstream `cholf5/stupid-dict` release `data-3`.
  Do not replace those URLs when updating this project's repository links.
- Run the affected package's tests first, then `make test` and `make vet`
  for Go code changes. Format touched Go files with `gofmt`.
- Build the CLI with `make build` (or `make build BINARY=/tmp/stupidict`).
  See [Build with Make](README.md#build-with-make) for metadata overrides;
  preserve the shared build information used by `-version` and About.
- Builds are pure Go (`CGO_ENABLED=0`); do not add CGO dependencies. Native
  playback uses Oto behind the `device` seam in `internal/speech`. For
  concurrency/lifecycle changes, run `make race` (the race detector itself needs
  CGO and a C compiler). Do not add external-command speech fallbacks.
- Do not launch the interactive app just to validate a code change: startup can
  download assets and write the default user profile. For manual runs, pass
  `-config <temporary-directory>/config.json` to isolate data and settings.
- See [Tests](README.md#tests) for optional real-asset tests and benchmarks.
  Ordinary tests need no downloaded assets; do not fetch them for unit tests.

## Test Patterns

- Extend neighboring tests and reuse `fixture` / `openFixture` in
  [dictionary_test.go](dictionary_test.go) for disposable SQLite data.
- Follow [internal/client/service_test.go](internal/client/service_test.go)
  for injected engines and local HTTP servers; keep tests off real user data
  and public download endpoints.
- Reuse the simulated screen, stub engine/speaker, and `startUI` / `wait`
  helpers in [internal/tui/app_test.go](internal/tui/app_test.go) for UI changes.
  Keep event dispatch and asynchronous assertions on that established path.
- Playback tests use decoded fixtures and the fake `device` in
  [internal/speech/output_test.go](internal/speech/output_test.go), not
  subprocesses or the real sound device (`STUPIDDICT_PLAYBACK=1` opts in).
  They cannot prove audible output. Report manual platform/audio verification
  separately; see [Offline speech](README.md#offline-speech).