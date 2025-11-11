# HLS Plugin Guide

## Table of Contents

- [Introduction](#introduction)
- [Key Features](#key-features)
- [Configuration](#configuration)
- [Live Playback Endpoints](#live-playback-endpoints)
- [Pulling Remote HLS Streams](#pulling-remote-hls-streams)
- [Recording and VOD](#recording-and-vod)
- [Built-in Player Assets](#built-in-player-assets)
- [Low-Latency HLS (LL-HLS)](#low-latency-hls-ll-hls)
- [Troubleshooting](#troubleshooting)

## Introduction

The HLS plugin turns Monibuca into an HTTP Live Streaming (HLS) origin, providing on-the-fly segmenting, in-memory caching, recording, download, and proxy capabilities. It exposes classic `.m3u8` playlists under `/hls`, supports time-ranged VOD playback, and can pull external HLS feeds back into the platform. The plugin ships with a pre-packaged `hls.js` demo so you can verify playback instantly.

## Key Features

- **Live HLS output** – Publish any stream in Monibuca and play it back at `http(s)://{host}/hls/{streamPath}.m3u8` with configurable segment length/window.
- **Adaptive in-memory cache** – Keeps the latest `Window` segments in memory; default `5s x 3` produces ~15 seconds of rolling content.
- **Remote HLS ingestion** – Pull `.m3u8` feeds, repackage the TS segments, and republish them through Monibuca.
- **Recording workflows** – Record HLS segments to disk, start/stop via config or REST API, and export TS/MP4/FMP4 archives.
- **Time-range download** – Generate stitched TS files from historical recordings or auto-convert MP4/FMP4 archives when TS is unavailable.
- **VOD playlists** – Serve historical playlists under `/vod/{streamPath}.m3u8` with support for `start`, `end`, or `range` queries.
- **Bundled player** – Access `hls.js` demos (benchmark, metrics, light UI) directly from the plugin without external hosting.

## Configuration

Basic YAML example:

```yaml
hls:
  onpub:
    transform:
      ^live/.+: 5s x 3        # fragment duration x playlist window
    record:
      ^live/.+:
        filepath: record/$0
        fragment: 1m          # split TS files every minute
  pull:
    live/apple-demo:
      url: https://devstreaming-cdn.apple.com/.../prog_index.m3u8
      relaymode: mix          # remux (default), relay, or mix
  onsub:
    pull:
      ^vod_hls_\d+/(.+)$: $1  # lazy pull VOD streams on demand
```

### Transform Input Format

`onpub.transform` accepts a string literal `{fragment} x {window}`. For example `5s x 3` tells the transformer to create 5-second segments and retain 3 completed segments (plus one in-progress) in-memory. Increase the window to improve DVR length; decrease it to lower latency.

### Recording Options

`onpub.record` uses the shared `config.Record` structure:

- `fragment` – TS file duration (`1m`, `10s`, etc.).
- `filepath` – Directory pattern; `$0` expands to the matched stream path.
- `realtime` – `true` writes segments as they are generated; `false` caches in memory and flushes when the segment closes.
- `append` – Append to existing files instead of creating new ones.

You can also trigger recordings dynamically via the HTTP API (see [Recording and VOD](#recording-and-vod)).

### Pull Parameters

`hls.pull` entries inherit from `config.Pull`:

- `url` – Remote playlist URL (HTTPS recommended).
- `maxretry`, `retryinterval` – Automatic retry behaviour (default 5 seconds between attempts).
- `proxy` – Optional HTTP proxy.
- `header` – Extra HTTP headers (cookies, tokens, user agents).
- `relaymode` – Choose how TS data is handled:
  - `remux` (default) – Decode TS to Monibuca tracks only; segments are regenerated.
  - `relay` – Cache raw TS in memory and skip remux/recording.
  - `mix` – Remux for playback and keep a copy of each TS segment in memory (best for redistributing the original segments).

## Live Playback Endpoints

- `GET /hls/{streamPath}.m3u8` – Rolling playlist. Add `?timeout=30s` to wait for a publisher to appear; the plugin auto-subscribes internally during the wait window.
- `GET /hls/{streamPath}/{segment}.ts` – TS segments held in memory or read from disk when available.
- `GET /hls/{resource}` – Any other path (e.g. `index.html`, `hls.js`) is served from the embedded `hls.js.zip` archive.

When Monibuca listens on standard ports, `PlayAddr` entries like `http://{host}/hls/live/demo.m3u8` and `https://{host}/hls/live/demo.m3u8` are announced automatically.

## Pulling Remote HLS Streams

1. Configure a `pull` block under the `hls` section or use the unified API:
   ```bash
   curl -X POST http://localhost:8080/api/stream/pull \
     -H "Content-Type: application/json" \
     -d '{
       "protocol": "hls",
       "streamPath": "live/apple-demo",
       "remoteURL": "https://devstreaming-cdn.apple.com/.../prog_index.m3u8"
     }'
   ```
2. The puller fetches the primary playlist, optionally follows variant streams, downloads the newest TS segments, and republishes them to Monibuca.
3. Choose `relaymode: mix` if you need the original TS bytes for downstream consumers (`MemoryTs` keeps a rolling window of segments).

Progress telemetry is available through the task system with step names `m3u8_fetch`, `parse`, and `ts_download`.

## Recording and VOD

### Start/Stop via REST

- `POST /hls/api/record/start/{streamPath}?fragment=30s&filePath=record/live` – Starts a TS recorder and returns a numeric task ID.
- `POST /hls/api/record/stop/{id}` – Stops the recorder (`id` is the value returned from the start call).

If no recorder exists for the same `streamPath` + `filePath`, the plugin creates one and persists metadata in the configured database (if enabled).

### Time-Range Playlists

- `GET /vod/{streamPath}.m3u8?start=2024-12-01T08:00:00&end=2024-12-01T09:00:00`
- `GET /vod/{streamPath}.m3u8?range=1700000000-1700003600`

The handler looks up matching `RecordStream` rows (`ts`, `mp4`, or `fmp4`) and builds a playlist that points either to existing files or to `/mp4/download/{stream}.fmp4?id={recordID}` for fMP4 archives.

### TS Download API

`GET /hls/download/{streamPath}.ts?start=1700000000&end=1700003600`

- If TS recordings exist, the plugin stitches them together, skipping duplicate PAT/PMT packets after the first file.
- When only MP4/FMP4 recordings are available, it invokes the MP4 demuxer, converts samples to TS on the fly, and streams the result to the client.
- Responses set `Content-Type: video/mp2t` and `Content-Disposition: attachment` so browsers download the merged file.

## Built-in Player Assets

Static assets bundled in `hls.js.zip` are served directly from `/hls`. Useful entry points:

- `/hls/index.html` – Full-featured `hls.js` demo.
- `/hls/index-light.html` – Minimal UI variant.
- `/hls/basic-usage.html` – Step-by-step example demonstrating basic playback controls.
- `/hls/metrics.html` – Visualise playlist latency, buffer length, and network metrics.

These pages load the local `/hls/hls.js` script, making it easy to sanity-check streams without external CDNs.

## Low-Latency HLS (LL-HLS)

The same package registers an `llhls` plugin for Low-Latency HLS output. Enable it by adding a transform:

```yaml
llhls:
  onpub:
    transform:
      ^live/.+: 1s x 7   # duration x segment count (SegmentMinDuration x SegmentCount)
```

LL-HLS exposes playlists at `http(s)://{host}/llhls/{streamPath}/index.m3u8` and keeps a `SegmentMinDuration` and `SegmentCount` tuned for sub-two-second glass-to-glass latency. The muxer automatically maps H.264/H.265 video and AAC audio using `gohlslib`.

## Troubleshooting

- **Playlist stays empty** – Confirm the publisher is active and that `onpub.transform` matches the stream path. Use `?timeout=30s` on the playlist request to give the server time to subscribe.
- **Segments expire too quickly** – Increase the transform window (e.g. `5s x 6`) or switch pull jobs to `relaymode: mix` to preserve original TS segments longer.
- **Download returns 404** – Ensure the database is enabled and recording metadata exists; the plugin relies on `RecordStream` entries to discover files.
- **Large time-range downloads stall** – The downloader streams sequentially; consider slicing the range or moving recordings to faster storage.
- **Access from browsers** – The `/hls` paths are plain HTTP GET endpoints. Configure CORS or a reverse proxy if you plan to fetch playlists from another origin.


