//go:build cluster

package plugin_cluster

import "time"

type ConsulConfig struct {
	Addresses            []string      `desc:"Consul HTTP API 地址列表,首个为主连接,其余暂未做客户端 failover"`
	Token                string        `desc:"可选,Consul ACL token"`
	WaitTime             time.Duration `default:"30s" desc:"KV blocking query 长轮询超时(P1)"`
	SessionTTL           time.Duration `default:"10s" desc:"Consul session TTL,Consul 硬性下限 10s"`
	SessionRenewInterval time.Duration `default:"3s" desc:"session 主动续期间隔"`
}

type AdvertiseConfig struct {
	RTMP string `desc:"对外宣告的 RTMP host:port,例如 10.0.0.5:1935"`
	RTSP string `desc:"对外宣告的 RTSP host:port,例如 10.0.0.5:554"`
	FLV  string `desc:"对外宣告的 HTTP-FLV 完整 URL,例如 http://10.0.0.5:8080"`
	GRPC string `desc:"对外宣告的 gRPC host:port"`
}

type LoadShedConfig struct {
	Enable          bool `desc:"是否启用负载卸载"`
	StreamThreshold int  `default:"500" desc:"流数阈值,超过此值在 lb-suggest 中降权"`
}

type MetricsConfig struct {
	ReportInterval time.Duration `default:"5s" desc:"LoadReporter 上报周期"`
}
