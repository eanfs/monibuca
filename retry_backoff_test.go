package m7s

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
)

// gotask 的指数退避 retryDelay = RetryInterval × 2^(retryCount-1),上限判断被
// `MaxRetryInterval > 0` 门控(task.go:338)——不调 SetMaxRetryInterval 就没有上限。
// 2026-07-17 实证:IPS 封禁 ~16h 后,三节点 30 路 puller 的 retryDelay 全部涨到
// 11h22m40s(5s×2^13),网络恢复后数小时不自愈,必须重启进程。
// 因此仓库约定:凡 SetRetry(非零重试)必须配 SetMaxRetryInterval(见 CLAUDE.md)。

type failingStartTask struct {
	task.Task
	tries atomic.Int32
}

func (f *failingStartTask) Start() error {
	f.tries.Add(1)
	return errors.New("boom")
}

// 起一个必然失败的任务,等它重试到 minTries 次,返回最后记录的 retryDelay。
func runFailingTask(t *testing.T, cap time.Duration, minTries int32) time.Duration {
	t.Helper()
	var root task.RootManager[uint32, *task.Work]
	var w task.Work
	root.Init()
	t.Cleanup(func() { root.Stop(task.ErrStopByUser) })
	root.AddTask(&w).WaitStarted()

	ft := &failingStartTask{}
	ft.SetRetry(-1, time.Millisecond)
	if cap > 0 {
		ft.GetTask().SetMaxRetryInterval(cap)
	}
	w.AddTask(ft)

	deadline := time.Now().Add(5 * time.Second)
	for ft.tries.Load() < minTries {
		if time.Now().After(deadline) {
			t.Fatalf("等待重试超时:tries=%d < %d(退避是否远超预期?)", ft.tries.Load(), minTries)
		}
		time.Sleep(2 * time.Millisecond)
	}
	v, ok := ft.GetDescription("retryDelay")
	if !ok {
		t.Fatal("未记录 retryDelay description")
	}
	d, err := time.ParseDuration(v.(string))
	if err != nil {
		t.Fatalf("retryDelay 解析失败: %v", err)
	}
	return d
}

// 不设上限:退避按 2^n 无界增长(第 10 次重试已达 1ms×2^9=512ms)。
// 上游 gotask 若改为默认封顶,本测试转红 = 4 处调用点的显式上限可考虑移除。
func TestRetryBackoffUnboundedWithoutCap(t *testing.T) {
	d := runFailingTask(t, 0, 10)
	if d < 256*time.Millisecond {
		t.Fatalf("无上限时第10次重试的退避应 >=512ms(2^9),实际 %v —— gotask 语义已变?", d)
	}
}

// 设上限:退避恒 <= MaxRetryInterval。这是 puller/pusher/transformer/cascade
// 各调用点补 SetMaxRetryInterval 的依据。
func TestRetryBackoffCappedWithMaxRetryInterval(t *testing.T) {
	cap := 8 * time.Millisecond
	d := runFailingTask(t, cap, 10)
	if d > cap {
		t.Fatalf("设上限 %v 后第10次重试退避 %v 仍超限", cap, d)
	}
}
