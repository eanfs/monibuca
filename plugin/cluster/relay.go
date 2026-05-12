//go:build cluster

package plugin_cluster

import (
	"fmt"
)

// buildPullURL 根据 RelayProtocols 优先级 + peer 的 Advertise 表,选第一个
// peer 真实在监听的协议,拼出 m7s Puller 可直接吃下的完整 URL。
//
// 协议惯例:
//   - rtmp: addr 是 host:port,完整 URL = rtmp://addr/streamPath
//   - rtsp: 同上,URL = rtsp://addr/streamPath
//   - flv : 配置里已经带 scheme(http:// 或 https://),URL = <flvAddr>/streamPath.flv
//
// 全部协议都没匹配上时返回 error,调用方记 Warn 后放弃 relay。
func buildPullURL(peer *PeerInfo, streamPath string, priority []string) (proto, fullURL string, err error) {
	for _, p := range priority {
		switch p {
		case "rtmp":
			if addr := peer.Advertise.RTMP; addr != "" {
				return "rtmp", "rtmp://" + addr + "/" + streamPath, nil
			}
		case "rtsp":
			if addr := peer.Advertise.RTSP; addr != "" {
				return "rtsp", "rtsp://" + addr + "/" + streamPath, nil
			}
		case "flv":
			if addr := peer.Advertise.FLV; addr != "" {
				return "flv", addr + "/" + streamPath + ".flv", nil
			}
		}
	}
	return "", "", fmt.Errorf("no advertised protocol matches priority %v for peer %s", priority, peer.NodeID)
}
