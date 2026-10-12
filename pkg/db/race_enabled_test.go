//go:build sqlite && race

package db

// raceEnabled: -race 插桩使所有操作慢 2-5 倍,压测延迟上界据此放宽。
const raceEnabled = true
