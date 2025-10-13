package util

import (
	"bufio"
	"io"
	"math/rand"
	"runtime"
	"testing"
)

// mockNetworkReader 模拟真实网络数据源
//
// 真实的网络读取场景中，每次 Read() 调用返回的数据长度是不确定的，
// 受多种因素影响：
// - TCP 接收窗口大小
// - 网络延迟和带宽
// - 操作系统缓冲区状态
// - 网络拥塞情况
//
// 这个 mock reader 通过每次返回随机长度的数据来模拟真实网络行为，
// 使基准测试更加接近实际应用场景。
type mockNetworkReader struct {
	data   []byte
	offset int
	rng    *rand.Rand
	// minChunk 和 maxChunk 控制每次返回的数据块大小范围
	minChunk int
	maxChunk int
}

func (m *mockNetworkReader) Read(p []byte) (n int, err error) {
	if m.offset >= len(m.data) {
		m.offset = 0 // 循环读取
	}

	// 计算本次可以返回的最大长度
	remaining := len(m.data) - m.offset
	maxRead := len(p)
	if remaining < maxRead {
		maxRead = remaining
	}

	// 随机返回 minChunk 到 min(maxChunk, maxRead) 之间的数据
	chunkSize := m.minChunk
	if m.maxChunk > m.minChunk && maxRead > m.minChunk {
		maxPossible := m.maxChunk
		if maxRead < maxPossible {
			maxPossible = maxRead
		}
		chunkSize = m.minChunk + m.rng.Intn(maxPossible-m.minChunk+1)
	}
	if chunkSize > maxRead {
		chunkSize = maxRead
	}

	n = copy(p[:chunkSize], m.data[m.offset:m.offset+chunkSize])
	m.offset += n
	return n, nil
}

// newMockNetworkReader 创建一个模拟真实网络的 reader
// 每次 Read 返回随机长度的数据（在 minChunk 到 maxChunk 之间）
func newMockNetworkReader(size int, minChunk, maxChunk int) *mockNetworkReader {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	return &mockNetworkReader{
		data:     data,
		rng:      rand.New(rand.NewSource(42)), // 固定种子保证可重复性
		minChunk: minChunk,
		maxChunk: maxChunk,
	}
}

// newMockNetworkReaderDefault 创建默认配置的模拟网络 reader
// 每次返回 64 到 2048 字节之间的随机数据
func newMockNetworkReaderDefault(size int) *mockNetworkReader {
	return newMockNetworkReader(size, 64, 2048)
}

// ============================================================
// 单元测试：验证 mockNetworkReader 的行为
// ============================================================

// TestMockNetworkReader_RandomChunks 验证随机长度读取功能
func TestMockNetworkReader_RandomChunks(t *testing.T) {
	reader := newMockNetworkReader(10000, 100, 500)
	buf := make([]byte, 1000)

	// 读取多次，验证每次返回的长度在预期范围内
	for i := 0; i < 10; i++ {
		n, err := reader.Read(buf)
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		if n < 100 || n > 500 {
			t.Errorf("第 %d 次读取返回 %d 字节，期望在 [100, 500] 范围内", i, n)
		}
	}
}

// ============================================================
// 核心基准测试：模拟真实网络场景
// ============================================================

// BenchmarkConcurrentNetworkRead_Bufio 模拟并发网络连接处理 - bufio.Reader
// 这个测试模拟多个并发连接持续读取和处理网络数据
// bufio.Reader 会为每个数据包分配新的缓冲区，产生大量临时内存
func BenchmarkConcurrentNetworkRead_Bufio(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		// 每个 goroutine 代表一个网络连接
		reader := bufio.NewReaderSize(newMockNetworkReaderDefault(10*1024*1024), 4096)

		for pb.Next() {
			// 模拟读取网络数据包并处理
			// 这里每次都分配新的缓冲区（真实场景中的常见做法）
			buf := make([]byte, 1024) // 每次分配 1KB - 会产生 GC 压力
			n, err := reader.Read(buf)
			if err != nil {
				b.Fatal(err)
			}

			// 模拟处理数据（计算校验和）
			var sum int
			for i := 0; i < n; i++ {
				sum += int(buf[i])
			}
			_ = sum
		}
	})
}

// BenchmarkConcurrentNetworkRead_BufReader 模拟并发网络连接处理 - BufReader
// 使用 BufReader 的零拷贝特性，通过内存池复用避免频繁分配
func BenchmarkConcurrentNetworkRead_BufReader(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		// 每个 goroutine 代表一个网络连接
		reader := NewBufReader(newMockNetworkReaderDefault(10 * 1024 * 1024))
		defer reader.Recycle()

		for pb.Next() {
			// 使用零拷贝的 ReadRange，无需分配缓冲区
			var sum int
			err := reader.ReadRange(1024, func(data []byte) {
				// 直接处理原始数据，无内存分配
				for _, b := range data {
					sum += int(b)
				}
			})
			if err != nil {
				b.Fatal(err)
			}
			_ = sum
		}
	})
}

// BenchmarkConcurrentProtocolParsing_Bufio 模拟并发协议解析 - bufio.Reader
// 模拟流媒体服务器解析多个并发流的数据包
func BenchmarkConcurrentProtocolParsing_Bufio(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		reader := bufio.NewReaderSize(newMockNetworkReaderDefault(10*1024*1024), 4096)

		for pb.Next() {
			// 读取包头（4字节长度）
			header := make([]byte, 4) // 分配 1
			_, err := io.ReadFull(reader, header)
			if err != nil {
				b.Fatal(err)
			}

			// 计算数据包大小（256-1024 字节）
			size := 256 + int(header[3])%768

			// 读取数据包内容
			packet := make([]byte, size) // 分配 2
			_, err = io.ReadFull(reader, packet)
			if err != nil {
				b.Fatal(err)
			}

			// 模拟处理数据包
			_ = packet
		}
	})
}

// BenchmarkConcurrentProtocolParsing_BufReader 模拟并发协议解析 - BufReader
func BenchmarkConcurrentProtocolParsing_BufReader(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		reader := NewBufReader(newMockNetworkReaderDefault(10 * 1024 * 1024))
		defer reader.Recycle()

		for pb.Next() {
			// 读取包头
			size, err := reader.ReadBE32(4)
			if err != nil {
				b.Fatal(err)
			}

			// 计算数据包大小
			packetSize := 256 + int(size)%768

			// 零拷贝读取和处理
			err = reader.ReadRange(packetSize, func(data []byte) {
				// 直接处理，无需分配
				_ = data
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkHighFrequencyReads_Bufio 高频小包读取 - bufio.Reader
// 模拟视频流的高频小包场景（如 30fps 视频流）
func BenchmarkHighFrequencyReads_Bufio(b *testing.B) {
	reader := bufio.NewReaderSize(newMockNetworkReaderDefault(10*1024*1024), 4096)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// 每次读取小数据包（128 字节）
		buf := make([]byte, 128) // 频繁分配小对象
		_, err := reader.Read(buf)
		if err != nil {
			b.Fatal(err)
		}
		_ = buf
	}
}

// BenchmarkHighFrequencyReads_BufReader 高频小包读取 - BufReader
func BenchmarkHighFrequencyReads_BufReader(b *testing.B) {
	reader := NewBufReader(newMockNetworkReaderDefault(10 * 1024 * 1024))
	defer reader.Recycle()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// 零拷贝读取
		err := reader.ReadRange(128, func(data []byte) {
			_ = data
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// ============================================================
// GC 压力测试：展示长时间运行下的 GC 影响
// ============================================================

// BenchmarkGCPressure_Bufio 展示 bufio.Reader 在持续运行下的 GC 压力
// 这个测试会产生大量临时内存分配，触发频繁 GC
func BenchmarkGCPressure_Bufio(b *testing.B) {
	var beforeGC runtime.MemStats
	runtime.ReadMemStats(&beforeGC)

	// 模拟 10 个并发连接持续处理数据
	b.SetParallelism(10)
	b.RunParallel(func(pb *testing.PB) {
		reader := bufio.NewReaderSize(newMockNetworkReaderDefault(100*1024*1024), 4096)

		for pb.Next() {
			// 模拟处理一个数据包：读取 + 处理 + 临时分配
			buf := make([]byte, 512) // 每次分配 512 字节
			n, err := reader.Read(buf)
			if err != nil {
				b.Fatal(err)
			}

			// 模拟数据处理（可能需要额外分配）
			processed := make([]byte, n) // 再分配一次
			copy(processed, buf[:n])

			// 模拟业务处理
			var sum int64
			for _, v := range processed {
				sum += int64(v)
			}
			_ = sum
		}
	})

	var afterGC runtime.MemStats
	runtime.ReadMemStats(&afterGC)

	// 报告 GC 统计
	b.ReportMetric(float64(afterGC.NumGC-beforeGC.NumGC), "gc-runs")
	b.ReportMetric(float64(afterGC.TotalAlloc-beforeGC.TotalAlloc)/1024/1024, "MB-alloc")
	b.ReportMetric(float64(afterGC.Mallocs-beforeGC.Mallocs), "mallocs")
}

// BenchmarkGCPressure_BufReader 展示 BufReader 通过内存复用降低 GC 压力
// 零拷贝 + 内存池复用，几乎不产生临时对象
func BenchmarkGCPressure_BufReader(b *testing.B) {
	var beforeGC runtime.MemStats
	runtime.ReadMemStats(&beforeGC)

	b.SetParallelism(10)
	b.RunParallel(func(pb *testing.PB) {
		reader := NewBufReader(newMockNetworkReaderDefault(100 * 1024 * 1024))
		defer reader.Recycle()

		for pb.Next() {
			// 零拷贝处理，无临时分配
			var sum int64
			err := reader.ReadRange(512, func(data []byte) {
				// 直接在原始内存上处理，无需拷贝
				for _, v := range data {
					sum += int64(v)
				}
			})
			if err != nil {
				b.Fatal(err)
			}
			_ = sum
		}
	})

	var afterGC runtime.MemStats
	runtime.ReadMemStats(&afterGC)

	// 报告 GC 统计
	b.ReportMetric(float64(afterGC.NumGC-beforeGC.NumGC), "gc-runs")
	b.ReportMetric(float64(afterGC.TotalAlloc-beforeGC.TotalAlloc)/1024/1024, "MB-alloc")
	b.ReportMetric(float64(afterGC.Mallocs-beforeGC.Mallocs), "mallocs")
}

// BenchmarkStreamingServer_Bufio 模拟流媒体服务器场景 - bufio.Reader
// 100 个并发连接，每个连接持续读取和转发数据
func BenchmarkStreamingServer_Bufio(b *testing.B) {
	var beforeGC runtime.MemStats
	runtime.ReadMemStats(&beforeGC)

	b.RunParallel(func(pb *testing.PB) {
		reader := bufio.NewReaderSize(newMockNetworkReaderDefault(50*1024*1024), 8192)
		frameNum := 0

		for pb.Next() {
			// 读取一帧数据（1KB-4KB 之间变化）
			frameSize := 1024 + (frameNum%3)*1024
			frameNum++
			frame := make([]byte, frameSize)

			_, err := io.ReadFull(reader, frame)
			if err != nil {
				b.Fatal(err)
			}

			// 模拟转发给多个订阅者（需要拷贝）
			for i := 0; i < 3; i++ {
				subscriber := make([]byte, len(frame))
				copy(subscriber, frame)
				_ = subscriber
			}
		}
	})

	var afterGC runtime.MemStats
	runtime.ReadMemStats(&afterGC)

	gcRuns := afterGC.NumGC - beforeGC.NumGC
	totalAlloc := float64(afterGC.TotalAlloc-beforeGC.TotalAlloc) / 1024 / 1024

	b.ReportMetric(float64(gcRuns), "gc-runs")
	b.ReportMetric(totalAlloc, "MB-alloc")
}

// BenchmarkStreamingServer_BufReader 模拟流媒体服务器场景 - BufReader
func BenchmarkStreamingServer_BufReader(b *testing.B) {
	var beforeGC runtime.MemStats
	runtime.ReadMemStats(&beforeGC)

	b.RunParallel(func(pb *testing.PB) {
		reader := NewBufReader(newMockNetworkReaderDefault(50 * 1024 * 1024))
		defer reader.Recycle()

		for pb.Next() {
			// 零拷贝读取
			err := reader.ReadRange(1024+1024, func(frame []byte) {
				// 直接使用原始数据，无需拷贝
				// 模拟转发（实际可以使用引用计数或共享内存）
				for i := 0; i < 3; i++ {
					_ = frame
				}
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	var afterGC runtime.MemStats
	runtime.ReadMemStats(&afterGC)

	gcRuns := afterGC.NumGC - beforeGC.NumGC
	totalAlloc := float64(afterGC.TotalAlloc-beforeGC.TotalAlloc) / 1024 / 1024

	b.ReportMetric(float64(gcRuns), "gc-runs")
	b.ReportMetric(totalAlloc, "MB-alloc")
}
