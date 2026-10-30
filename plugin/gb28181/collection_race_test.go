package plugin_gb28181pro

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	gb28181 "m7s.live/v5/plugin/gb28181/pkg"
)

// newChannelRaceFixture 构造目录处理路径所需的最小状态：一个插件实例 + 若干设备。
// 不调用 GB28181Plugin.Start（Start 需要 DB/SIP 监听，且会自行初始化插件级集合锁），
// 以便复现“设备初始化先于/独立于插件 Start”的场景。
func newChannelRaceFixture(t *testing.T, deviceCount int) (*GB28181Plugin, []*Device) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gb := &GB28181Plugin{}
	gb.Logger = logger
	devices := make([]*Device, deviceCount)
	for i := range devices {
		d := &Device{DeviceId: fmt.Sprintf("3402000000132000%04d", i)}
		d.plugin = gb
		d.Logger = logger.With("deviceid", d.DeviceId)
		devices[i] = d
	}
	return gb, devices
}

func channelID(d *Device, i int) string {
	return fmt.Sprintf("%s_channel%03d", d.DeviceId, i)
}

// TestConcurrentChannelUpdate 并发跑真实的目录更新路径（addOrUpdateChannel），
// 同时有读者读取插件级 plugin.channels，-race 下不得报数据竞争。
// 这对应 10-18 崩溃栈：util.(*Collection).Get <- Device.addOrUpdateChannel <- catalogHandlerTask.Run。
func TestConcurrentChannelUpdate(t *testing.T) {
	const (
		deviceCount  = 8
		channelsEach = 40
		readerCount  = 4
	)
	gb, devices := newChannelRaceFixture(t, deviceCount)

	// 先完成设备初始化（StoreDevice / RecoverDevice / checkDeviceExpire 都会走到
	// ensureCollectionMutex）。此后才允许并发目录处理与集合读取——这正是待验证的不变量：
	// 设备初始化必须把插件级 channels 的锁也准备好。修复前该处恒为 nil，下面的并发即触发
	// Collection 裸读写（10-18 的 concurrent map access）。
	for _, d := range devices {
		d.ensureCollectionMutex()
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < readerCount; r++ {
		readers.Add(1)
		go func(r int) {
			defer readers.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				d := devices[(r+i)%deviceCount]
				// 与 addOrUpdateChannel 内部的读一致：按 ID 查插件级集合
				gb.channels.Get(channelID(d, i%channelsEach))
				// 与 API/清理路径一致：遍历插件级集合
				gb.channels.Range(func(*Channel) bool { return true })
			}
		}(r)
	}

	var writers sync.WaitGroup
	for _, d := range devices {
		writers.Add(1)
		go func(d *Device) {
			defer writers.Done()
			for i := 0; i < channelsEach; i++ {
				d.addOrUpdateChannel(gb28181.DeviceChannel{
					ID:        channelID(d, i),
					DeviceId:  d.DeviceId,
					ChannelId: fmt.Sprintf("channel%03d", i),
				})
			}
		}(d)
	}
	writers.Wait()
	close(stop)
	readers.Wait()

	if got, want := gb.channels.Length, deviceCount*channelsEach; got != want {
		t.Fatalf("plugin.channels length = %d, want %d", got, want)
	}
}

// TestPluginChannelsMutexInitialized 断言设备初始化后插件级 channels 集合的锁非 nil，
// 且重复初始化幂等（返回同一个锁实例）。
func TestPluginChannelsMutexInitialized(t *testing.T) {
	gb, devices := newChannelRaceFixture(t, 1)
	d := devices[0]
	if gb.channels.L != nil {
		t.Fatal("fixture precondition failed: plugin channels lock should start nil")
	}

	d.ensureCollectionMutex()
	if gb.channels.L == nil {
		t.Fatal("plugin-level channels lock is nil after device initialization")
	}
	first := gb.channels.L
	if d.channels.L == nil {
		t.Fatal("device-level channels lock is nil after device initialization")
	}
	if d.catalogReqs.L == nil {
		t.Fatal("device-level catalogReqs lock is nil after device initialization")
	}

	d.ensureCollectionMutex()
	if gb.channels.L != first {
		t.Fatal("plugin-level channels lock was re-created; initialization must be idempotent")
	}
}
