//go:build cluster

package plugin_cluster

import "errors"

var (
	// ErrOriginLost 是 §4.2 中 Relay 主动 Stop 本节点 cluster-relay pull-proxy 时用的
	// reason。Lookup 探测到 streamPath 已不在任何节点上时,触发本 error。
	ErrOriginLost = errors.New("cluster: origin node lost")

	// ErrStreamPathTaken 是 §4.3 first-write-wins 失败时 publisher.Stop 用的 reason。
	// 本节点试图 acquire 一个 streamPath,但 KV 上已被另一个 session 持有。
	ErrStreamPathTaken = errors.New("cluster: streamPath already owned by peer")
)
