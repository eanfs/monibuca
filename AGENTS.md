# Repository Guidelines

## Project Structure & Module Organization

- `/*.go`: core server/framework entrypoints and types (module: `m7s.live/v5`).
- `pkg/`: shared building blocks (config, codecs/formats, utilities).
- `plugin/`: built-in plugins, one per folder (e.g., `plugin/rtsp/`, `plugin/webrtc/`); see `plugin/README.md`.
- `pb/` and `plugin/*/pb/`: Protocol Buffer `.proto` definitions and generated Go code.
- `example/`: runnable examples and YAML configs (recommended starting point for local dev).
- `test/` and `**/*_test.go`: integration and unit tests.
- `doc/` and `doc_CN/`: architecture and design notes.

## Build, Test, and Development Commands

- Go toolchain: follow `go.mod` (includes `toolchain go1.24.10`).
- Run an example server: `cd example/default && go run -tags sqlite main.go`.
- Run all tests: `go test ./...` (add tags when needed, e.g. `go test -tags sqlite ./...`).
- Build locally: `go build ./...` (or the example entrypoint: `go build -tags "sqlite mysql postgres" ./example/default`).
- Regenerate protobufs: `sh scripts/protoc.sh` (or `sh scripts/protoc.sh <plugin_name>`; requires `protoc` + `protoc-gen-go`).
- Basic checks: `gofmt -w path/to/file.go && go vet ./...` (optional: `staticcheck ./...`, configured by `staticcheck.conf`).
- Release build (maintainers): `goreleaser build` (uses `goreleaser.yml`).

## Coding Style & Naming Conventions

- Format Go code with `gofmt` (tabs/indentation handled automatically).
- Keep package names short and lowercase; file names follow existing patterns like `server_http.go`.
- Prefer small, focused diffs; avoid sweeping refactors or repo-wide formatting changes.

## Testing Guidelines

- Use Go’s `testing` package; some tests also use `github.com/stretchr/testify`.
- Add/adjust `*_test.go` alongside the package you change; use table-driven tests where practical.
- Run a targeted test while iterating: `go test ./plugin/rtsp/... -run TestName`.

## Commit & Pull Request Guidelines

- Commit subjects follow a Conventional Commits style seen in history: `feat: ...`, `fix: ...`, `chore: ...`, `doc: ...`, optional scope like `feat(codec): ...`.
- PRs should include: what/why, how to test (commands), and config snippets (typically under `example/`) when behavior changes.
- If UI behavior changes, include screenshots; avoid committing secrets or large binaries (the Web UI expects an external `admin.zip` placed next to the runtime config).
