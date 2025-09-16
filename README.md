<!-- Improved compatibility of back to top link -->
<a id="readme-top"></a>

[![Contributors][contributors-shield]][contributors-url]
[![Forks][forks-shield]][forks-url]
[![Stargazers][stars-shield]][stars-url]
[![Issues][issues-shield]][issues-url]
[![MIT License][license-shield]][license-url]

<!-- PROJECT LOGO -->
<br />
<div align="center">
<a href="https://m7s.live">
    <img src="https://m7s.live/logo+.svg" alt="Logo" width="200">
  </a>
  <h1 align="center">Monibuca v5</h1>

  <p align="center">
    A highly scalable high-performance streaming server development framework developed purely in Go
    <br />
    <a href="./README_CN.md">中文文档</a>
    ·
    <a href="https://github.com/Monibuca/v5/wiki"><strong>Explore the docs »</strong></a>
    <br />
    <br />
    <a href="https://github.com/Monibuca/v5/issues">Report Bug</a>
    ·
    <a href="https://github.com/Monibuca/v5/issues">Request Feature</a>
  </p>
</div>

<!-- TABLE OF CONTENTS -->
<details>
  <summary>Table of Contents</summary>
  <ol>
    <li><a href="#about">About</a></li>
    <li><a href="#getting-started">Getting Started</a></li>
    <li><a href="#usage">Usage</a></li>
    <li><a href="#build-tags">Build Tags</a></li>
    <li><a href="#monitoring">Monitoring</a></li>
    <li><a href="#plugin-development">Plugin Development</a></li>
    <li><a href="#arch">Architecture</a></li>
    <li><a href="#third-party-plugins">Third-party Plugins</a></li>
    <li><a href="#contributing">Contributing</a></li>
    <li><a href="#license">License</a></li>
    <li><a href="#contact">Contact</a></li>
  </ol>
</details>

## About

Monibuca is a powerful streaming server framework written entirely in Go. It's designed to be:

- 🚀 **High Performance** - Lock-free design, partial manual memory management, multi-core computing
- ⚡ **Low Latency** - Zero-wait forwarding, sub-second latency across the entire chain
- 📦 **Modular** - Load on demand, unlimited extensibility
- 🔧 **Flexible** - Highly configurable to meet various streaming scenarios
- 💪 **Scalable** - Supports distributed deployment, easily handles large-scale scenarios
- 🔍 **Debug Friendly** - Built-in debug plugin, real-time performance monitoring and analysis
- 🎥 **Media Processing** - Supports screenshot, transcoding, SEI data processing
- 🔄 **Cluster Capability** - Built-in cascade and room management
- 🎮 **Preview Features** - Supports video preview, multi-screen preview, custom screen layouts
- 🔐 **Security** - Provides encrypted transmission and stream authentication
- 📊 **Performance Monitoring** - Supports stress testing and performance metrics collection (integrated in test plugin)
- 📝 **Log Management** - Log rotation, auto cleanup, custom extensions
- 🎬 **Recording & Playback** - Supports MP4, HLS, FLV formats, speed control, seeking, pause
- ⏱️ **Dynamic Time-Shift** - Dynamic cache design, supports live time-shift playback
- 🌐 **Remote Call** - Supports gRPC interface for cross-language integration
- 🏷️ **Stream Alias** - Supports dynamic stream alias, flexible multi-stream management
- 🤖 **AI Capabilities** - Integrated inference engine, ONNX model support, custom pre/post processing
- 🪝 **WebHook** - Subscribe to stream lifecycle events for business system integration
- 🔒 **Private Protocol** - Supports custom private protocols for special business needs

- 🔄 **Supported Protocols**: RTMP, RTSP, HTTP-FLV, WS-FLV, HLS, WebRTC, GB28181, ONVIF, SRT

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Getting Started

### Prerequisites

- Go 1.23 or higher
- Basic understanding of streaming protocols

### Run with Default Configuration

```bash
cd example/default
go run -tags sqlite main.go
```

### Web UI

Place the `admin.zip` file (do not unzip) in the same directory as your configuration file.

Then visit http://localhost:8080 to access the UI.

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Examples

For more examples, please check out the [example](./example) documentation.

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Build Tags

The following build tags can be used to customize your build:

| Build Tag | Description |
|-----------|-------------|
| disable_rm | Disables the memory pool |
| sqlite | Enables the sqlite DB |  
| sqliteCGO | Enables the sqlite cgo version DB |
| mysql | Enables the mysql DB |
| postgres | Enables the postgres DB |
| duckdb | Enables the duckdb DB |
| taskpanic | Throws panic, for testing |
| fasthttp | Enables the fasthttp server instead of net/http |
| enable_buddy | Enables the buddy memory pre-allocation |

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Monitoring

Monibuca supports Prometheus monitoring out of the box. Add the following to your Prometheus configuration:

```yaml
scrape_configs:
  - job_name: "monibuca"
    metrics_path: "/api/metrics"
    static_configs:
      - targets: ["localhost:8080"]
```

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Plugin Development

Monibuca's functionality can be extended through plugins. For information on creating plugins, see the [plugin guide](./plugin/README.md).

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Architecture

For detailed architecture design documentation, please refer to the [Architecture Documentation](./doc/arch/index.md).

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Third-party Plugins

- https://github.com/cuteLittleDevil/m7s-jt1078

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## Contributing

Contributions are what make the open source community such an amazing place to learn, inspire, and create. Any contributions you make are **greatly appreciated**.

1. Fork the Project
2. Create your Feature Branch (`git checkout -b feature/AmazingFeature`)
3. Commit your Changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the Branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

<p align="right">(<a href="#readme-top">back to top</a>)</p>

## License

Distributed under the AGPL License. See `LICENSE` for more information.

<p align="right">(<a href="#readme-top">back to top</a>)</p>

<!-- CONTACT -->
## Contact

monibuca - [@m7server](https://x.com/m7server) - service@monibuca.com

Project Link: [https://github.com/langhuihui/monibuca](https://github.com/langhuihui/monibuca)

<p align="right">(<a href="#readme-top">back to top</a>)</p>


<!-- MARKDOWN LINKS & IMAGES -->
[contributors-shield]: https://img.shields.io/github/contributors/langhuihui/monibuca.svg?style=for-the-badge
[contributors-url]: https://github.com/langhuihui/monibuca/graphs/contributors
[forks-shield]: https://img.shields.io/github/forks/langhuihui/monibuca.svg?style=for-the-badge
[forks-url]: https://github.com/langhuihui/monibuca/network/members
[stars-shield]: https://img.shields.io/github/stars/langhuihui/monibuca.svg?style=for-the-badge
[stars-url]: https://github.com/langhuihui/monibuca/stargazers
[issues-shield]: https://img.shields.io/github/issues/langhuihui/monibuca.svg?style=for-the-badge
[issues-url]: https://github.com/langhuihui/monibuca/issues
[license-shield]: https://img.shields.io/github/license/langhuihui/monibuca.svg?style=for-the-badge
[license-url]: https://github.com/langhuihui/monibuca/blob/v5/LICENSE
