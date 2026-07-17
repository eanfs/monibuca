package m7s

import (
	"testing"
	"time"

	task "github.com/eanfs/gotask"
)

// gotask 的 OnStart(task.go:286)只把 listener 追加进 afterStartListeners，不检查任务
// 是否已启动；该切片仅在启动流程(task.go:377)消费一次。因此 AddTask 之后再 OnStart，
// 回调永远不会触发 —— UploadRetryScheduler / CheckSubWaitTimeout 都是这样注册没的，
// 结果补传子系统整体静默失效(上传失败的录像永远留在 pending 目录不再补传)。
func TestOnStartAfterAddTaskNeverFires(t *testing.T) {
	var root task.RootManager[uint32, *task.Work]
	var work task.Work
	root.Init()
	t.Cleanup(func() { root.Stop(task.ErrStopByUser) })

	root.AddTask(&work).WaitStarted()

	fired := make(chan struct{}, 1)
	work.OnStart(func() { fired <- struct{}{} })

	select {
	case <-fired:
		t.Fatal("OnStart 在任务启动后注册竟然触发了：gotask 语义已变，" +
			"server.go 的注册顺序约束可以放宽")
	case <-time.After(300 * time.Millisecond):
		// 符合预期：注册晚了就永远不触发 —— 所以必须在 AddTask 之前注册
	}
}

// 对照：AddTask 之前注册的 OnStart 必然触发。这是 server.go 里
// UploadRetryScheduler / CheckSubWaitTimeout 必须遵守的注册顺序。
func TestOnStartBeforeAddTaskFires(t *testing.T) {
	var root task.RootManager[uint32, *task.Work]
	var work task.Work
	root.Init()
	t.Cleanup(func() { root.Stop(task.ErrStopByUser) })

	fired := make(chan struct{}, 1)
	work.OnStart(func() { fired <- struct{}{} })

	root.AddTask(&work).WaitStarted()

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("AddTask 之前注册的 OnStart 未触发")
	}
}
