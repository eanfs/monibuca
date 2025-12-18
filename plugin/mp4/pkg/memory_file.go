package mp4

import (
	"bytes"
	"io"
	"os"
)

type MemoryFile struct {
	*bytes.Buffer
	pos int64
}

func NewMemoryFile(buf *bytes.Buffer) *MemoryFile {
	if buf == nil {
		buf = bytes.NewBuffer(nil)
	}
	return &MemoryFile{Buffer: buf}
}

func (m *MemoryFile) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = m.pos + offset
	case io.SeekEnd:
		newPos = int64(m.Buffer.Len()) + offset
	default:
		return 0, os.ErrInvalid
	}

	if newPos < 0 {
		return 0, os.ErrInvalid
	}

	if newPos > int64(m.Buffer.Len()) {
		// Extend buffer if seeking beyond end
		m.Buffer.Write(make([]byte, newPos-int64(m.Buffer.Len())))
	}

	m.pos = newPos
	return m.pos, nil
}

func (m *MemoryFile) Read(p []byte) (n int, err error) {
	if m.pos >= int64(m.Buffer.Len()) {
		return 0, io.EOF
	}
	n = copy(p, m.Buffer.Bytes()[m.pos:])
	m.pos += int64(n)
	if n < len(p) {
		err = io.EOF
	}
	return
}

func (m *MemoryFile) Write(p []byte) (n int, err error) {
	// If writing beyond current size, extend the buffer
	if m.pos > int64(m.Buffer.Len()) {
		m.Buffer.Write(make([]byte, m.pos-int64(m.Buffer.Len())))
	}

	// If writing at the end, use Buffer.Write
	if m.pos == int64(m.Buffer.Len()) {
		n, err = m.Buffer.Write(p)
		m.pos += int64(n)
		return
	}

	// Otherwise, copy data at the current position
	n = copy(m.Buffer.Bytes()[m.pos:], p)
	if n < len(p) {
		// If we need more space, extend the buffer
		m.Buffer.Write(p[n:])
		n = len(p)
	}
	m.pos += int64(n)
	return
}
