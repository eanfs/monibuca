package mp4

import (
	"path/filepath"
	"sync"
)

// activeIndexRegistry 记录本进程内仍由活跃 recorder 持有的 sidecar/媒体路径。
//
// 崩溃恢复此前只靠 mtime 新鲜度判断「是否仍在录制」。一旦活跃录制超过
// minage 没有新样本（拉流重连退避、低帧率、上游抖动），就会被误判为崩溃残留，
// sidecar 被 rename、媒体被恢复产物覆盖、源文件被删除，而 recorder 继续写
// 已 unlink 的 inode，后续数据全部丢失。进程内活跃集合是权威判据：只要
// recorder 仍持有索引，恢复侧必须跳过，无论 mtime 多旧。
//
// 归一化用绝对路径 + Clean：sidecar 路径可能来自不同调用点（相对/绝对），
// 必须落到同一个键上。跨进程无效——另一个进程的活跃录制只能靠 mtime 保护，
// 这也是恢复扫描仍保留新鲜度闸门的原因。
var activeIndexRegistry sync.Map // normalized path -> struct{}

func normalizeActiveIndexPath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

// RegisterActiveIndex 把一段录制正在使用的 sidecar 与媒体路径登记为活跃。
// indexPath 为空时只登记 mediaPath。同一路径重复登记是幂等的。
func RegisterActiveIndex(indexPath, mediaPath string) {
	for _, path := range []string{indexPath, mediaPath} {
		if key := normalizeActiveIndexPath(path); key != "" {
			activeIndexRegistry.Store(key, struct{}{})
		}
	}
}

// UnregisterActiveIndex 在录制结束（commit/close）后注销活跃登记。
// 只有注销之后，恢复扫描才允许定稿该 sidecar。
func UnregisterActiveIndex(indexPath, mediaPath string) {
	for _, path := range []string{indexPath, mediaPath} {
		if key := normalizeActiveIndexPath(path); key != "" {
			activeIndexRegistry.Delete(key)
		}
	}
}

// IsActiveRecordingPath 报告任一给定路径是否仍被本进程的活跃 recorder 持有。
func IsActiveRecordingPath(paths ...string) bool {
	for _, path := range paths {
		if key := normalizeActiveIndexPath(path); key != "" {
			if _, ok := activeIndexRegistry.Load(key); ok {
				return true
			}
		}
	}
	return false
}
