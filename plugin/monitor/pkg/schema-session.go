package monitor

import (
	"database/sql"
	"time"
)

type Session struct {
	ID        uint32 `gorm:"primarykey"`
	PID       int
	Args      string
	StartTime time.Time
	EndTime   sql.NullTime
}
