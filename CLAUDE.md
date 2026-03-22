# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the **v4 instance project** (启动工程) for Monibuca (m7s), a high-performance streaming server framework written in Go. This repo is NOT the engine or plugin source — it imports `m7s.live/engine/v4` and official plugins via Go modules, assembles them, and provides configuration. The core engine and plugins live in the [Monibuca GitHub org](https://github.com/Monibuca).

**Note:** Active development has moved to monibuca-v5. This v4 project is maintained for compatibility.

## Architecture

Monibuca v4 has a three-layer architecture:

1. **Engine** (`m7s.live/engine/v4`) — Provides stream caching/forwarding, publish-subscribe infrastructure, plugin lifecycle management, HTTP API auto-registration, event bus, memory pooling, and authentication. The engine does not implement any specific protocol.

2. **Plugins** — Each plugin is a separate Go module imported via blank identifier (`_ "m7s.live/plugin/xxx/v4"`). Plugins provide protocol implementations (RTMP, RTSP, HLS, WebRTC, GB28181, etc.), recording, logging, monitoring, and other features. The `plugin-record` directory contains a custom fork (`github.com/eanfs/plugin-record/v4`) used instead of the official record plugin.

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

## Important Notes

- Go 1.21+ required
- CGO is disabled for builds (`CGO_ENABLED=0`)
- `plugin-record/` is a local module override — changes here affect recording behavior directly
- Crash logs are written to `fatal/` directory — check `fatal.log` if the process exits unexpectedly
- When using ffmpeg to push streams, always specify codecs: `-c:v h264 -c:a aac`
- WebRTC does not support AAC audio or H.265 video (browser limitation)
