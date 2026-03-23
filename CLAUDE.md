# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the **v4 instance project** (启动工程) for Monibuca (m7s), a high-performance streaming server framework written in Go. This repo is NOT the engine or plugin source — it imports `m7s.live/engine/v4` and official plugins via Go modules, assembles them, and provides configuration. The core engine and plugins live in the [Monibuca GitHub org](https://github.com/Monibuca).

**Note:** Active development has moved to monibuca-v5. This v4 project is maintained for compatibility.

## Architecture

Monibuca v4 has a three-layer architecture:

1. **Engine** (`m7s.live/engine/v4`) — Provides stream caching/forwarding, publish-subscribe infrastructure, plugin lifecycle management, HTTP API auto-registration, event bus, memory pooling, and authentication. The engine does not implement any specific protocol.

2. **Plugins** — Each plugin is a separate Go module imported via blank identifier (`_ "m7s.live/plugin/xxx/v4"`). Plugins provide protocol implementations (RTMP, RTSP, HLS, WebRTC, GB28181, etc.), recording, logging, monitoring, and other features. The `plugin-record/` directory contains a custom fork (`github.com/eanfs/plugin-record/v4`) used instead of the official record plugin.

3. **Instance Project** (this repo) — `main.go` imports the engine and desired plugins, reads `config.yaml`, and starts the server. Plugin selection is done entirely through Go imports — adding/removing a plugin means adding/removing its import line.

### Key Design Patterns

- **Zero-config startup**: All plugins are enabled by default. `config.yaml` only needs entries for values you want to override.
- **Plugin enable/disable**: Any plugin can be disabled by setting `enable: false` in its config section.
- **Stream path format**: Must be two-level, e.g., `live/test`. Single-level or leading-slash paths like `/live` are invalid.
- **On-demand pull**: Streams can be pulled on first subscriber request. Use `delayclosetimeout` to auto-stop when no subscribers remain.

## Common Commands

### Run locally
```bash
go run .
# or with custom config:
go run . -c path/to/config.yaml
```

### Build
```bash
go build -ldflags="-s -w" -o monibuca ./main.go
```

### Release build (cross-platform via GoReleaser)
```bash
goreleaser build
```

### Run tests
```bash
cd test && go test ./...
# Single test:
cd test && go test -run TestPubAndSub -v
# Benchmark:
cd test && go test -bench BenchmarkPubAndSub
```

### Docker
```bash
# Build
docker build -t monibuca .
# Run (expose all protocol ports)
docker run -id -p 1935:1935 -p 8080:8080 -p 8443:8443 -p 554:554 -p 58200:58200 -p 5060:5060/udp -p 8000:8000/udp monibuca
```

### Code generation
```bash
go generate  # runs gen.go
```

## Configuration

- Config file: `config.yaml` (or set `M7S_CONFIG_FILE` env var)
- Example configs for specific protocols: `conf-example/`
- Default HTTP API gateway: `:8080`, HTTPS: `:8443`
- Protocol ports: RTMP `:1935`, RTSP `:554`, WebRTC UDP `:8000-9000`, GB28181 SIP `:5060/udp`, GB28181 media `:58200-59200`

## plugin-record Custom Fork

The `plugin-record/` directory is a local module override of `github.com/eanfs/plugin-record/v4`. Changes here directly affect recording behavior. It extends the official record plugin with:

### Recording Modes
- **OrdinaryMode** (0): Continuous recording, triggered by `autorecord` config or the API `/record/api/start`
- **EventMode** (1): Event-driven recording with before/after duration buffers (default 30s each), triggered via `/record/api/event_start`

### Database
- Default: SQLite at `./m7sv4.db` (configured via `SqliteDbPath`)
- Alternative: MySQL via `MysqlDSN` config field
- Uses GORM with auto-migration for two tables: `EventRecord` (recording sessions) and `Exception` (upload/system failures), plus `FLVKeyframe` (VOD keyframe index)

### Minio/S3 Storage
Configured under `record.storage` in `config.yaml`. When configured, files are uploaded after recording with exponential backoff retry. Failed uploads are stored in the `Exception` table for retry via `runFailedUploadRetrier`.

### Key REST API Endpoints (all under `/record/api/`)
| Endpoint | Method | Description |
|---|---|---|
| `start` | GET | Start recording (params: `streamPath`, `type`, `fileName`, `fragment`, `duration`) |
| `stop` | GET | Stop recording by recorder ID |
| `stop_stream` | GET | Stop all recordings for a stream path |
| `list` | GET | List recorded files by type |
| `list_page` | GET | Paginated file list |
| `list_recording` | GET | List active in-progress recordings |
| `event_start` | POST | Start event recording (requires `token: m7s` header) |
| `event_list` | POST | Query event recording history |
| `alarm_list` | POST | Query upload exception records |
| `recordfile_delete` | GET | Delete a recording file (path-traversal protected) |
| `recordfile_modify` | GET | Rename a recording file |

### VOD Playback
- `GET /record/play/flv/{streamPath}?start=20060102150405&end=20060102150405&speed=1` — Live-stream a time range from fragmented FLV files
- `GET /record/download/flv/{streamPath}?start=...&end=...` — Download a spliced FLV with correct keyframe index

### Auto-Recovery
`checker.go` runs every 5 minutes, compares DB active recordings vs in-memory recorders, and auto-restarts any that stopped unexpectedly.

### File Naming
- Fragmented recordings: `{streamPath}/{channel}-{timestamp}.{ext}`
- Single-file recordings: `{streamPath}/{fileName}.{ext}` (or timestamp-based if no fileName)
- `RecordPathNotShowStreamPath: true` (default) omits stream path prefix from file path

## Important Notes

- Go 1.21+ required
- CGO is disabled for builds (`CGO_ENABLED=0`)
- `plugin-record/` is a local module override — changes here affect recording behavior directly
- Crash logs are written to `fatal/` directory — check `fatal.log` if the process exits unexpectedly
- When using ffmpeg to push streams, always specify codecs: `-c:v h264 -c:a aac`
- WebRTC does not support AAC audio or H.265 video (browser limitation)
- Event API endpoints (`event_start`, `event_list`, `alarm_list`) require `token: m7s` HTTP header — this is a hardcoded token, not configurable
