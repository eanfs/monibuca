# RTSP Plugin

The RTSP plugin provides complete RTSP server and client functionality for Monibuca, enabling RTSP stream publishing, playback, and proxying.

## Features

- **RTSP Server**: Accept RTSP client connections for stream publishing and playback
- **RTSP Client**: Pull streams from remote RTSP sources
- **Dual Transport Modes**: Support both TCP and UDP transport protocols
- **Authentication**: Built-in username/password authentication
- **Bidirectional Proxy**: Support for both pull and push proxying
- **Standard Compliance**: Implements RTSP protocol (RFC 2326/RFC 7826)

## Configuration

```yaml
rtsp:
  tcp:
    listenaddr: :554        # RTSP server listening address
  username: ""              # Authentication username (optional)
  password: ""              # Authentication password (optional)
  udpport: 20001-30000      # UDP port range for media transmission
```

### Configuration Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `tcp.listenaddr` | string | `:554` | RTSP server listening address and port |
| `username` | string | `""` | Authentication username (empty = no auth) |
| `password` | string | `""` | Authentication password |
| `udpport` | range | `20001-30000` | UDP port range for RTP/RTCP transmission |

## Usage

### Pull RTSP Stream

Pull a remote RTSP stream into Monibuca:

```yaml
rtsp:
  pull:
    camera1:
      url: rtsp://admin:password@192.168.1.100/stream
```

Or use the unified API:
```bash
curl -X POST http://localhost:8080/api/stream/pull \
  -H "Content-Type: application/json" \
  -d '{
    "protocol": "rtsp",
    "streamPath": "camera1",
    "remoteURL": "rtsp://admin:password@192.168.1.100/stream"
  }'
```

### Push Stream via RTSP

Push a stream from Monibuca to a remote RTSP server:

```yaml
rtsp:
  push:
    camera1:
      target: rtsp://192.168.1.200/live/stream
```

### Publish Stream to Monibuca RTSP Server

Use FFmpeg or other RTSP clients to publish streams:

```bash
ffmpeg -re -i input.mp4 -c copy -f rtsp rtsp://localhost:554/live/stream
```

### Play Stream from Monibuca RTSP Server

Use any RTSP client to play streams:

```bash
ffplay rtsp://localhost:554/live/stream
```

Or with VLC, OBS, or other RTSP-compatible players.

## Transport Modes

### TCP Transport (Interleaved Mode)

- More reliable, works through firewalls
- Higher latency
- Automatic fallback option

### UDP Transport

- Lower latency
- Better for local networks
- Requires open UDP port range
- RTP/RTCP use separate ports

## RTSP Methods Supported

| Method | Direction | Description |
|--------|-----------|-------------|
| OPTIONS | Both | Query supported methods |
| DESCRIBE | Pull | Get stream SDP information |
| ANNOUNCE | Push | Declare stream for publishing |
| SETUP | Both | Setup transport parameters |
| PLAY | Pull | Start playing stream |
| RECORD | Push | Start recording/publishing |
| TEARDOWN | Both | Close connection |

## Authentication

When username and password are configured, the server requires HTTP Basic Authentication:

```yaml
rtsp:
  username: admin
  password: secret123
```

Clients must provide credentials in the URL:
```
rtsp://admin:secret123@localhost:554/live/stream
```

## Advanced Features

### Pull Proxy

Automatically pull and cache remote RTSP streams. Configure under the `global` node:

```yaml
global:
  pullproxy:
    - id: 1                          # Unique ID, must be > 0
      name: "camera-1"               # Pull proxy name
      type: "rtsp"                   # Protocol type
      streampath: "live/camera1"     # Stream path in Monibuca
      pullonstart: true              # Auto-pull on startup
      pull:
        url: "rtsp://admin:password@192.168.1.100/stream"
      description: "Front door camera"
    - id: 2
      name: "camera-2"
      type: "rtsp"
      streampath: "live/camera2"
      pullonstart: false
      pull:
        url: "rtsp://admin:password@192.168.1.101/stream"
```

Or use the API to manage pull proxies dynamically (see API Endpoints section below).

### Push Proxy

Automatically push streams to remote RTSP servers. Configure under the `global` node:

```yaml
global:
  pushproxy:
    - id: 1                          # Unique ID, must be > 0
      name: "push-1"                 # Push proxy name
      type: "rtsp"                   # Protocol type
      streampath: "live/stream1"     # Source stream path
      pushonstart: true              # Auto-push on startup
      push:
        url: "rtsp://192.168.1.200/live/stream1"
      description: "Push to remote server"
```

Or use the API to manage push proxies dynamically (see API Endpoints section below).

## Compatibility

### Tested Devices/Software

- ✅ FFmpeg
- ✅ VLC Media Player
- ✅ OBS Studio
- ✅ ONVIF-compliant devices

### Known Issues

See [BAD_DEVICE.md](BAD_DEVICE.md) for devices with non-standard RTSP implementations.

## Codec Support

The plugin transparently passes through codec information. Supported codecs depend on the source and destination:

**Video**: H.264, H.265/HEVC, MPEG-4, MJPEG  
**Audio**: AAC, G.711 (PCMA/PCMU), G.726, MP3, OPUS

## Performance Tips

- Use TCP transport for streams over the internet
- Use UDP transport for local network streams (lower latency)
- Adjust UDP port range based on concurrent stream count
- Enable authentication to prevent unauthorized access
- For high-concurrency scenarios, consider hardware transcoding

## Troubleshooting

### Connection Refused
- Check if port 554 requires root/administrator privileges
- Try a different port (e.g., 8554)
- Verify firewall settings

### No Video/Audio
- Check codec compatibility between source and client
- Verify SDP information in logs
- Test with VLC or FFplay to isolate issues

### UDP Packet Loss
- Increase UDP port range
- Switch to TCP transport
- Check network quality and bandwidth

## API Endpoints

### Pull Stream

Use the unified pull API with `protocol` set to `rtsp`:

```bash
POST /api/stream/pull
Content-Type: application/json

{
  "protocol": "rtsp",
  "streamPath": "camera1",
  "remoteURL": "rtsp://admin:password@192.168.1.100/stream",
  "pubAudio": true,
  "pubVideo": true
}
```

**Parameters:**
- `protocol` (required): Set to `"rtsp"`
- `streamPath` (required): Local stream path in Monibuca
- `remoteURL` (required): Remote RTSP URL to pull from
- `pubAudio` (optional): Enable audio publishing
- `pubVideo` (optional): Enable video publishing
- `testMode` (optional): 0 = normal pull, 1 = pull without publishing
- Additional publish configuration options available (see GlobalPullRequest in global.proto)

### Stop Stream

```bash
POST /api/stream/stop/{streamPath}
```

### Manage Pull/Push Proxy

- `GET /api/proxy/pull/list` - List all pull proxies
- `POST /api/proxy/pull/add` - Add a pull proxy
- `POST /api/proxy/pull/update` - Update a pull proxy
- `POST /api/proxy/pull/remove/{id}` - Remove a pull proxy
- `GET /api/proxy/push/list` - List all push proxies
- `POST /api/proxy/push/add` - Add a push proxy
- `POST /api/proxy/push/update` - Update a push proxy
- `POST /api/proxy/push/remove/{id}` - Remove a push proxy

## Acknowledgments

This plugin references code and implementation ideas from the excellent [go2rtc](https://github.com/AlexxIT/go2rtc) project by AlexxIT, which provides a comprehensive media server solution with advanced streaming capabilities.

## License

This plugin is part of the Monibuca project and follows the same license terms.

