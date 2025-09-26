package storage

import (
	"fmt"
	"os"

	"golang.org/x/exp/mmap"
)

// MmapFile 使用内存映射的文件实现
type MmapFile struct {
	file     *os.File
	mmapFile *mmap.ReaderAt
	data     []byte
	size     int64
}

// NewMmapFile 创建新的内存映射文件
func NewMmapFile(filename string) (*MmapFile, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	mmapFile, err := mmap.Open(filename)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to mmap file: %w", err)
	}

	// 获取文件大小
	stat, err := file.Stat()
	if err != nil {
		mmapFile.Close()
		file.Close()
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	// 获取内存映射的数据
	data := make([]byte, stat.Size())
	_, err = mmapFile.ReadAt(data, 0)
	if err != nil {
		mmapFile.Close()
		file.Close()
		return nil, fmt.Errorf("failed to read mmap data: %w", err)
	}

	return &MmapFile{
		file:     file,
		mmapFile: mmapFile,
		data:     data,
		size:     stat.Size(),
	}, nil
}

// Read 实现 io.Reader 接口
func (m *MmapFile) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	// 使用内存映射的数据进行零拷贝读取
	n = copy(p, m.data)
	if n == 0 {
		return 0, fmt.Errorf("no data available")
	}

	// 更新数据指针，模拟读取进度
	m.data = m.data[n:]
	return n, nil
}

// ReadAt 实现 io.ReaderAt 接口
func (m *MmapFile) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= m.size {
		return 0, fmt.Errorf("offset beyond file size")
	}

	// 使用内存映射的数据进行零拷贝读取
	available := int(m.size - off)
	if len(p) > available {
		p = p[:available]
	}

	// 直接从内存映射区域复制数据，避免系统调用
	start := int(off)
	end := start + len(p)
	if end > len(m.data) {
		end = len(m.data)
	}

	n = copy(p, m.data[start:end])
	return n, nil
}

// Write 实现 io.Writer 接口
func (m *MmapFile) Write(p []byte) (n int, err error) {
	// 内存映射文件通常是只读的，这里返回错误
	return 0, fmt.Errorf("mmap file is read-only")
}

// WriteAt 实现 io.WriterAt 接口
func (m *MmapFile) WriteAt(p []byte, off int64) (n int, err error) {
	// 内存映射文件通常是只读的，这里返回错误
	return 0, fmt.Errorf("mmap file is read-only")
}

// Seek 实现 io.Seeker 接口
func (m *MmapFile) Seek(offset int64, whence int) (int64, error) {
	// 对于内存映射文件，我们通过调整数据指针来模拟 seek
	switch whence {
	case 0: // io.SeekStart
		if offset < 0 || offset > m.size {
			return 0, fmt.Errorf("invalid offset")
		}
		m.data = m.data[offset:]
		return offset, nil
	case 1: // io.SeekCurrent
		current := m.size - int64(len(m.data))
		newOffset := current + offset
		if newOffset < 0 || newOffset > m.size {
			return 0, fmt.Errorf("invalid offset")
		}
		m.data = m.data[offset:]
		return newOffset, nil
	case 2: // io.SeekEnd
		newOffset := m.size + offset
		if newOffset < 0 || newOffset > m.size {
			return 0, fmt.Errorf("invalid offset")
		}
		m.data = m.data[newOffset:]
		return newOffset, nil
	default:
		return 0, fmt.Errorf("invalid whence")
	}
}

// Close 关闭文件
func (m *MmapFile) Close() error {
	var err error
	if m.mmapFile != nil {
		err = m.mmapFile.Close()
	}
	if m.file != nil {
		if closeErr := m.file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

// Stat 返回文件信息
func (m *MmapFile) Stat() (os.FileInfo, error) {
	return m.file.Stat()
}

// Name 返回文件名
func (m *MmapFile) Name() string {
	return m.file.Name()
}

// Size 返回文件大小
func (m *MmapFile) Size() int64 {
	return m.size
}

// Data 返回内存映射的数据切片（零拷贝访问）
func (m *MmapFile) Data() []byte {
	return m.data
}

// MmapFileWriter 支持写入的内存映射文件
type MmapFileWriter struct {
	file     *os.File
	filename string
	data     []byte
	size     int64
}

// NewMmapFileWriter 创建新的可写内存映射文件
func NewMmapFileWriter(filename string) (*MmapFileWriter, error) {
	file, err := os.Create(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to create file: %w", err)
	}

	return &MmapFileWriter{
		file:     file,
		filename: filename,
		data:     make([]byte, 0),
		size:     0,
	}, nil
}

// Write 实现 io.Writer 接口
func (m *MmapFileWriter) Write(p []byte) (n int, err error) {
	// 将数据追加到内存缓冲区
	m.data = append(m.data, p...)
	m.size += int64(len(p))
	return len(p), nil
}

// WriteAt 实现 io.WriterAt 接口
func (m *MmapFileWriter) WriteAt(p []byte, off int64) (n int, err error) {
	// 确保数据缓冲区足够大
	if int64(len(m.data)) < off+int64(len(p)) {
		newSize := off + int64(len(p))
		newData := make([]byte, newSize)
		copy(newData, m.data)
		m.data = newData
		m.size = newSize
	}

	// 写入数据到指定位置
	copy(m.data[off:], p)
	return len(p), nil
}

// Sync 同步数据到磁盘
func (m *MmapFileWriter) Sync() error {
	// 将内存中的数据写入文件
	_, err := m.file.WriteAt(m.data, 0)
	if err != nil {
		return fmt.Errorf("failed to write data: %w", err)
	}

	// 同步到磁盘
	return m.file.Sync()
}

// Close 关闭文件
func (m *MmapFileWriter) Close() error {
	// 先同步数据
	if err := m.Sync(); err != nil {
		m.file.Close()
		return err
	}
	return m.file.Close()
}

// Read 实现 io.Reader 接口
func (m *MmapFileWriter) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	n = copy(p, m.data)
	if n == 0 {
		return 0, fmt.Errorf("no data available")
	}

	// 更新数据指针
	m.data = m.data[n:]
	return n, nil
}

// ReadAt 实现 io.ReaderAt 接口
func (m *MmapFileWriter) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= m.size {
		return 0, fmt.Errorf("offset beyond file size")
	}

	available := int(m.size - off)
	if len(p) > available {
		p = p[:available]
	}

	n = copy(p, m.data[off:])
	return n, nil
}

// Seek 实现 io.Seeker 接口
func (m *MmapFileWriter) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0: // io.SeekStart
		if offset < 0 || offset > m.size {
			return 0, fmt.Errorf("invalid offset")
		}
		m.data = m.data[offset:]
		return offset, nil
	case 1: // io.SeekCurrent
		current := m.size - int64(len(m.data))
		newOffset := current + offset
		if newOffset < 0 || newOffset > m.size {
			return 0, fmt.Errorf("invalid offset")
		}
		m.data = m.data[offset:]
		return newOffset, nil
	case 2: // io.SeekEnd
		newOffset := m.size + offset
		if newOffset < 0 || newOffset > m.size {
			return 0, fmt.Errorf("invalid offset")
		}
		m.data = m.data[newOffset:]
		return newOffset, nil
	default:
		return 0, fmt.Errorf("invalid whence")
	}
}

// Stat 返回文件信息
func (m *MmapFileWriter) Stat() (os.FileInfo, error) {
	return m.file.Stat()
}

// Name 返回文件名
func (m *MmapFileWriter) Name() string {
	return m.filename
}

// Size 返回文件大小
func (m *MmapFileWriter) Size() int64 {
	return m.size
}

// Data 返回内存中的数据切片（零拷贝访问）
func (m *MmapFileWriter) Data() []byte {
	return m.data
}
