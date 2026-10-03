# Contributing to Purlview

Thank you for helping. Issues and pull requests are welcome; for anything
larger than a fix, please open an issue first so that we can agree on the
approach before you spend time on it. Report security issues privately, as
[SECURITY.md](SECURITY.md) describes.

## Developer Certificate of Origin

Purlview does not ask for a contributor licence agreement. Instead, every
commit carries a `Signed-off-by` line certifying the
[Developer Certificate of Origin](https://developercertificate.org/): that
you wrote the change, or otherwise have the right to submit it under the
project's licence, Apache-2.0.

```sh
git commit -s
```

adds the line from your Git name and email. Commits without it cannot be
merged.

## Building

You need Go 1.27.1 or later (with `GOTOOLCHAIN=auto`, Go 1.21 or later
fetches it) and Git. Every dependency is a public Go module.

```sh
go build -trimpath -o purlview ./cmd/purlview
./purlview --help
```

## Testing

```sh
go vet ./...
go test -race ./...
go mod tidy -diff
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run
```

On Windows on Arm, run the tests without `-race`. The tests run on macOS,
Linux and Windows and need nothing beyond Go: they build real CLI and daemon
executables, start daemons, run the installer scripts against a loopback
release server and serve a synthetic platform through `sdk/apiserver`.

The tests never touch your own daemon, credential or home directory. Every
daemon and command they run uses a private `PURLVIEW_STATE_DIR`; inside a
`go test` binary the default per-user paths are refused
(`daemon.ErrUnisolatedTest`); and the installer tests run the scripts only
with a throwaway home directory (`install/isolation_test.go`). Keep it that
way in new tests: use `t.Setenv` for anything a started daemon must inherit.

### Output, goldens and transcripts

Only `internal/output` writes to the process streams; a lint rule and
`internal/output/streams_test.go` enforce it. When you change what a command
prints:

```sh
go test ./internal/command -run TestGoldens -update   # rewrites testdata/golden
go run ./internal/tools/scenario doc -write docs/cli-scenarios.md
```

and commit the regenerated files with the change. `go test ./...` fails when
either is stale. `go run ./internal/tools/scenario list` and
`go run ./internal/tools/scenario run <name>` run single scenarios.

### The SDK's wire examples

`sdk/api/testdata` holds the serialised examples of the account API. Every
example must decode strictly, validate and encode back to the same bytes, so
a change to an API type and its example land together.

## Continuous integration

`.github/workflows/ci.yml` builds, vets, lints and tests on Linux, macOS and
Windows for every pull request. It needs no secrets.

## Style

- Comments explain contracts, invariants, ownership, concurrency, security,
  or a non-obvious reason in the code. Keep them true without context from
  outside this repository.
- Errors and output follow the patterns in `internal/output`: one line,
  "problem — remedy", with the cause only under `PURLVIEW_DEBUG=1`.
- Tests use synthetic identities and reserved domains (`example.invalid`,
  `purlview.invalid`), never real accounts or hosts.
- Use UK English in prose; identifiers keep their established spelling.
