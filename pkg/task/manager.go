package task

import (
	"errors"

	. "m7s.live/v5/pkg/util"
)

var ErrExist = errors.New("exist")

type ManagerItem[K comparable] interface {
	ITask
	GetKey() K
}

type Manager[K comparable, T ManagerItem[K]] struct {
	Work
	Collection[K, T]
}

func (m *Manager[K, T]) Add(ctx T, opt ...any) *Task {
	ctx.OnStart(func() {
		if !m.Collection.AddUnique(ctx) {
			ctx.Stop(ErrExist)
			return
		}
		if m.Logger != nil {
			m.Logger.Debug("add", "key", ctx.GetKey(), "count", m.Length)
		}
	})
	ctx.OnDispose(func() {
		m.Remove(ctx)
		if m.Logger != nil {
			m.Logger.Debug("remove", "key", ctx.GetKey(), "count", m.Length)
		}
	})
	return m.AddTask(ctx, opt...)
}

// SafeGet 用于不同协程获取元素，防止并发请求
func (m *Manager[K, T]) SafeGet(key K) (item T, ok bool) {
	if m.L == nil {
		m.Call(func() error {
			item, ok = m.Collection.Get(key)
			return nil
		})
	} else {
		item, ok = m.Collection.Get(key)
	}
	return
}

// SafeRange 用于不同协程获取元素，防止并发请求
func (m *Manager[K, T]) SafeRange(f func(T) bool) {
	if m.L == nil {
		m.Call(func() error {
			m.Collection.Range(f)
			return nil
		})
	} else {
		m.Collection.Range(f)
	}
}
