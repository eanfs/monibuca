//go:build cluster

package plugin_cluster

import "testing"

// TestRelayShouldPullNow 钉住"首次订阅必须立刻拉流"的判据：
// 只有新建成功的 relay proxy 才立即 Pull；复用已存在的 proxy 不重复 Pull
// （其 PullJob 由 Server.OnSubscribe 的兜底与 Tick 维护）。
//
// 背景（2026-11-06 三机真机复现）：relay proxy 创建后状态是 Offline，而
// Server.OnSubscribe 只对 Status==Online 的 proxy 触发 Pull()，于是首次订阅者
// 必然等到 subscribe timeout（10s），第二次订阅才拿到流。修复是在创建成功的
// 同一时刻拉起 PullJob。
func TestRelayShouldPullNow(t *testing.T) {
	if !relayShouldPullNow(true) {
		t.Fatal("newly created relay proxy must be pulled immediately")
	}
	if relayShouldPullNow(false) {
		t.Fatal("reused relay proxy must not be pulled again")
	}
}
