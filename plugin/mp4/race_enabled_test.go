//go:build race

package plugin_mp4

// raceEnabled 报告测试是否在 -race 下编译。用于跳过会触发 gotask v1.0.5
// 已知内部数据竞争的用例（与 pkg/db 的同名常量同源）。
const raceEnabled = true
