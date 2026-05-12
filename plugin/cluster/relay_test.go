//go:build cluster

package plugin_cluster

import (
	"testing"
)

// ---------------------------------------------------------------------
// buildPullURL: 纯函数,根据 RelayProtocols 优先级 + peer 的 Advertise 表
// 选第一个可用协议,拼成完整 pull URL。不需 consul,纯单元测试。
// ---------------------------------------------------------------------

func TestRelay_BuildPullURL_PicksRTMPWhenAvailable(t *testing.T) {
	peer := &PeerInfo{
		NodeID: "node-a",
		Advertise: AdvertiseConfig{
			RTMP: "10.0.0.1:1935",
			RTSP: "10.0.0.1:554",
			FLV:  "http://10.0.0.1:8080",
		},
	}
	proto, fullURL, err := buildPullURL(peer, "live/foo", []string{"rtmp", "rtsp", "flv"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "rtmp" {
		t.Errorf("proto = %q, want rtmp", proto)
	}
	if fullURL != "rtmp://10.0.0.1:1935/live/foo" {
		t.Errorf("url = %q, want rtmp://10.0.0.1:1935/live/foo", fullURL)
	}
}

func TestRelay_BuildPullURL_FallsBackWhenFirstProtocolMissing(t *testing.T) {
	peer := &PeerInfo{
		NodeID: "node-a",
		Advertise: AdvertiseConfig{
			RTSP: "10.0.0.1:554",
		},
	}
	proto, fullURL, err := buildPullURL(peer, "live/foo", []string{"rtmp", "rtsp", "flv"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "rtsp" {
		t.Errorf("proto = %q, want rtsp", proto)
	}
	if fullURL != "rtsp://10.0.0.1:554/live/foo" {
		t.Errorf("url = %q, want rtsp://10.0.0.1:554/live/foo", fullURL)
	}
}

func TestRelay_BuildPullURL_FLVUsesAdvertisedSchemeAndAppendsDotFLV(t *testing.T) {
	peer := &PeerInfo{
		NodeID: "node-a",
		Advertise: AdvertiseConfig{
			FLV: "http://10.0.0.1:8080",
		},
	}
	proto, fullURL, err := buildPullURL(peer, "live/foo", []string{"rtmp", "rtsp", "flv"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proto != "flv" {
		t.Errorf("proto = %q, want flv", proto)
	}
	if fullURL != "http://10.0.0.1:8080/live/foo.flv" {
		t.Errorf("url = %q, want http://10.0.0.1:8080/live/foo.flv", fullURL)
	}
}

func TestRelay_BuildPullURL_NoMatchingProtocolReturnsError(t *testing.T) {
	peer := &PeerInfo{Advertise: AdvertiseConfig{}}
	_, _, err := buildPullURL(peer, "live/foo", []string{"rtmp", "rtsp", "flv"})
	if err == nil {
		t.Fatalf("expected error when no protocol matches, got nil")
	}
}
