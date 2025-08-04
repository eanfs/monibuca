package debug

import (
	"database/sql"
	"time"
)

// Session 表示一个监控会话
type Session struct {
	ID        uint32 `gorm:"primarykey"`
	PID       int
	Args      string
	StartTime time.Time
	EndTime   sql.NullTime
}

// Task 表示一个任务记录
type Task struct {
	ID                          uint `gorm:"primarykey"`
	SessionID, TaskID, ParentID uint32
	StartTime, EndTime          time.Time
	OwnerType                   string
	TaskType                    byte
	Description                 string
	Reason                      string
	Level                       byte
}