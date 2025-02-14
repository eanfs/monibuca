package box

type Sample struct {
	KeyFrame  bool
	Data      []byte
	Timestamp uint32
	CTS       uint32
	Offset    int64
	Size      int
}
