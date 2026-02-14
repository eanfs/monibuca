# AGENTS.md
Guidance for autonomous coding agents working in this repository (`m7s.live/v5`).

## Repository facts
- Language: Go (`go 1.24`, `toolchain go1.24.10`)
- Module: `m7s.live/v5`
- Architecture: core server + plugins + gotask lifecycle (`Task/Job/Work`)
- Main runnable example: `example/default/main.go`
- Plugin code: `plugin/*`
- Shared libs: `pkg/*`
- Proto definitions: `pb/` and `plugin/*/pb/`

## Build and run commands
- Run default app (from `example/default`):
  - `go run -tags sqlite main.go`
- Build all packages:
  - `go build ./...`
- Build with tags (examples):
  - `go build -tags sqlite ./...`
  - `go build -tags "mysql fasthttp" ./...`
- Release-style local build:
  - `goreleaser build`

## Test commands
- Run all tests:
  - `go test ./...`
- Run all tests (verbose):
  - `go test -v ./...`
- Run one package:
  - `go test ./plugin/rtsp/pkg`
  - `go test ./pkg/config`
- Run a single test (exact name):
  - `go test ./test -run '^TestRestart$' -count=1`
  - `go test ./plugin/rtsp/pkg -run '^TestConnection$' -count=1`
- Run by regex (test/subtest group):
  - `go test ./pkg/config -run 'TestGlobal' -count=1`
- Run benchmarks only:
  - `go test ./pkg/util -bench . -run '^$'`
- Run with race detector (slow):
  - `go test -race ./...`

## Lint / static analysis / formatting
- Format changed Go files:
  - `gofmt -w <changed-files>`
- Basic static checks:
  - `go vet ./...`
- Staticcheck config exists (`staticcheck.conf`):
  - `staticcheck ./...`
- Deep static analysis config exists:
  - `qodana.yaml` (Qodana Go)

## Proto generation
- Generate all global proto outputs:
  - `sh scripts/protoc.sh`
- Generate a specific plugin proto output:
  - `sh scripts/protoc.sh <plugin_name>`
- Example:
  - `sh scripts/protoc.sh mp4`

## Supported/important build tags
- `sqlite`
- `sqliteCGO`
- `mysql`
- `postgres`
- `duckdb`
- `disable_rm`
- `taskpanic`
- `fasthttp`
- `enable_buddy`

## Cursor and Copilot rules
Detected Cursor rule file: `.cursor/rules/monibuca.mdc`

Rule content summary:
- If `.proto` files are changed and compilation is needed, use scripts under `scripts/`.

Required behavior for agents:
- Prefer `sh scripts/protoc.sh` (global) or `sh scripts/protoc.sh <plugin_name>` (plugin).
- Do not start with ad-hoc raw `protoc` command lines.

Detected Copilot instructions:
- `.github/copilot-instructions.md` not found during analysis.

## Code style guidelines

### Imports
- Use standard Go import grouping; let `gofmt` order imports.
- Keep aliases only when needed for clarity or conflict avoidance.
- Dot imports are generally discouraged.
- Exception: `staticcheck.conf` whitelists `. "m7s.live/v5/pkg"`.
- Do not introduce new dot imports outside the whitelist.

### Formatting
- Always run `gofmt` on touched Go files.
- Keep changes idiomatic; avoid cosmetic-only churn.
- Keep comments concise and only for non-obvious logic.

### Naming
- Exported identifiers use PascalCase (`Server`, `PluginMeta`, `InstallPlugin`).
- Unexported identifiers use camelCase (`loadAdminZip`, `checkInterval`).
- Error sentinels use `ErrXxx` style (`ErrRestart`, `ErrNoDB`).
- Plugin package naming often follows `plugin_xxx`; keep existing local pattern.

### Types and config structs
- Config structs commonly use tags like `default:"..."` and `desc:"..."`.
- Preserve tag keys and semantics when extending config.
- Prefer existing embedding patterns (`m7s.Plugin`, `task.Task`, `task.Work`).
- Favor consistency with nearby code over introducing new abstractions.

### Error handling
- Return errors explicitly; avoid silent failure.
- Use `errors.Is` for sentinel error checks where appropriate.
- Include context in logs and wrapped/constructed errors.
- Fail fast in startup/init paths when required dependencies are invalid.
- In task flows, respect lifecycle semantics (`Start`, `Run`, `Dispose`, stop reasons).

### Logging
- Follow existing structured logging style (`p.Info`, `p.Error`, `s.Warn`, etc.).
- Prefer key/value context (`error`, `path`, `streamPath`, `type`, ...).
- Keep log messages short; avoid redundant prose.

### Plugin and async patterns
- Register plugins through `m7s.InstallPlugin[YourPlugin](m7s.PluginMeta{...})`.
- Implement plugin startup in `Start() (err error)` style.
- Register HTTP routes via `RegisterHandler() map[string]http.HandlerFunc` when needed.
- Prefer managed tasks (`AddTask`, `WaitStarted`, `WaitStopped`) over bare goroutines.
- Use queue-style work managers (`task.Work`) for background serial processing.

### Test style
- Keep tests in `*_test.go` near related source packages.
- Use `testing` idioms (`t.Run`, focused checks).
- Iterate with single test/package first, then run broader suites.

## Agent workflow checklist
- Identify affected package/plugin and required build tags before coding.
- Keep patch scope small; avoid unrelated refactors.
- After edits (minimum):
  - `gofmt -w <changed-files>`
  - `go test <affected-package> -count=1`
- Before finalizing substantial work:
  - `go test ./...`
  - optional: `go vet ./...` and `staticcheck ./...`

## Common pitfalls
- Forgetting required build tags for DB/protocol-specific behavior.
- Editing `.proto` but not regenerating with `scripts/protoc.sh`.
- Adding non-whitelisted dot imports.
- Bypassing task lifecycle conventions in async plugin code.
- Mixing behavior changes and broad cleanup in one patch.

## Quick command cheat sheet
- Full tests: `go test ./...`
- Single package tests: `go test ./path/to/pkg`
- Single test: `go test ./path/to/pkg -run '^TestName$' -count=1`
- Format: `gofmt -w <files>`
- Vet: `go vet ./...`
- Staticcheck: `staticcheck ./...`
- Run default app: `(cd example/default && go run -tags sqlite main.go)`
- Proto all: `sh scripts/protoc.sh`
- Proto one plugin: `sh scripts/protoc.sh <plugin_name>`

Keep this file updated when workflows, tooling, or repo rules change.
