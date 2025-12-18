package box

import "github.com/langhuihui/gomem"

type Sample struct {
	gomem.Memory
	KeyFrame  bool
	Timestamp uint32
	CTS       uint32
	Offset    int64
	Duration  uint32
}
