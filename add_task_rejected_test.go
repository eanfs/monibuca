package m7s

import (
	"context"
	"testing"

	task "github.com/eanfs/gotask"
)

// addTaskRejectedChild 是只用来占位的子任务（无 Start/Run/Go）。
type addTaskRejectedChild struct {
	task.Task
}

// 键冲突 / 事件循环输入通道满（ErrTooManyChildren）这两类 AddTask 同步拒绝
// 不会取消任务上下文，只会把任务从父 Job 的子任务表里删掉。AddTaskRejected
// 必须据此识别，否则调用方会把“被拒”当成“已入队”，预留的名额永久泄漏。
//
// 这里直接构造两种形态（已登记 / 未登记）钉住识别逻辑：真正的 ErrTooManyChildren
// 需要把事件循环输入通道灌满，无法在单测里确定性复现。
func TestAddTaskRejectedDetectsUnregisteredTask(t *testing.T) {
	var root task.RootManager[uint32, task.ManagerItem[uint32]]
	root.Init()
	s := &Server{}
	if err := root.AddTask(&s.Records).WaitStarted(); err != nil {
		t.Fatalf("start Records: %v", err)
	}

	child := &addTaskRejectedChild{}
	registered := s.Records.AddTask(child)
	if registered == nil {
		t.Fatal("AddTask returned nil task")
	}
	if AddTaskRejected(&s.Records, registered) {
		t.Fatal("task that AddTask accepted must not be reported as rejected")
	}

	// 未被 AddTask 登记、但上下文仍然有效的任务：即键冲突/通道满被拒后的形态。
	orphan := &task.Task{Context: context.Background()}
	if !AddTaskRejected(&s.Records, orphan) {
		t.Fatal("unregistered task with a live context must be reported as rejected")
	}

	// 已运行完（上下文已取消）且已从子任务表移除的任务不是“同步拒绝”。
	doneCtx, doneCancel := context.WithCancel(context.Background())
	doneCancel()
	done := &task.Task{Context: doneCtx}
	if AddTaskRejected(&s.Records, done) {
		t.Fatal("a stopped task must not be reported as a synchronous AddTask rejection")
	}

	if AddTaskRejected(&s.Records, nil) {
		t.Fatal("nil task must not be reported as rejected")
	}
}
