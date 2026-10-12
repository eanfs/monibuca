//go:build sqlite

package db

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type stressRec struct {
	ID    uint `gorm:"primaryKey"`
	Name  string
	Value int
}

// TestSQLiteStress_NoWALConcurrentReadWrite 模拟生产形态下的并发读写压力:
//   - DSN 用 buildSQLiteDSN(no-WAL + busy_timeout,即修复后默认)
//   - 连接池复刻 server.go:335-337 的 MaxOpenConns(1)/MaxIdleConns(1)
//   - 24 写者 + 24 读者并发,各 100 次操作(对标 31 路录制入库/上传状态写
//     并发叠加 API 查询的生产峰值形态)
//
// 断言三件事,钉死"去 WAL 不引入读写卡死"::
//  1. 零 "database is locked"/"busy" —— 单连接池排队 + busy_timeout 兜底有效
//  2. 全部操作在 watchdog 期限内完成 —— 无死锁/饿死
//  3. 单操作最大延迟有界 —— rollback journal + synchronous=FULL 的 fsync
//     变重不至于把媒体路径 DB 操作拖过 recoder.go 的 2s 超时
func TestSQLiteStress_NoWALConcurrentReadWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过压力测试")
	}

	dsn := buildSQLiteDSN(filepath.Join(t.TempDir(), "stress.db"))
	// 只检 query 部分:t.TempDir() 路径含测试名 "NoWAL",不能整串匹配
	if _, query, _ := strings.Cut(dsn, "?"); strings.Contains(strings.ToLower(query), "wal") {
		t.Fatalf("压测前置断言失败:DSN 参数不应含 WAL: %s", dsn)
	}
	gdb, err := gorm.Open(gormlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("db(): %v", err)
	}
	// 复刻 server.go:335-337 —— sqlite 单连接池,读写全串行排队
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	if err = gdb.AutoMigrate(&stressRec{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const (
		writers  = 24
		readers  = 24
		opsEach  = 100
		deadline = 120 * time.Second // watchdog:超时即判卡死
	)
	// 延迟上界对齐生产真实红线:recoder.go 媒体路径 DB 超时 2s(超时仅 WRN
	// 不断录)。48 热循环 goroutine 抢 1 连接是远超生产的极端排队形态
	// (31 路录制的写入在时间上稀疏),实测本机 -race 下峰值 ~1.1s。
	// -race 插桩使操作慢 2-5 倍,上界放宽 3 倍防慢 CI 误报。
	maxOpLatency := 2 * time.Second
	if raceEnabled {
		maxOpLatency *= 3
	}

	var (
		wg        sync.WaitGroup
		lockedCnt atomic.Int64 // "database is locked/busy" 次数
		otherErrs atomic.Int64
		firstErr  atomic.Value // 首个非锁错误样本
		maxLatNs  atomic.Int64 // 单操作最大延迟
	)
	recordLatency := func(d time.Duration) {
		ns := d.Nanoseconds()
		for {
			old := maxLatNs.Load()
			if ns <= old || maxLatNs.CompareAndSwap(old, ns) {
				return
			}
		}
	}
	recordErr := func(err error) {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "locked") || strings.Contains(msg, "busy") {
			lockedCnt.Add(1)
		} else {
			otherErrs.Add(1)
			firstErr.CompareAndSwap(nil, err.Error())
		}
	}

	start := time.Now()
	for w := range writers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range opsEach {
				opStart := time.Now()
				err := gdb.Create(&stressRec{Name: "writer", Value: id*opsEach + i}).Error
				recordLatency(time.Since(opStart))
				if err != nil {
					recordErr(err)
				}
			}
		}(w)
	}
	for r := range readers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for range opsEach {
				opStart := time.Now()
				var cnt int64
				err := gdb.Model(&stressRec{}).Where("value >= ?", 0).Count(&cnt).Error
				if err == nil {
					var rows []stressRec
					err = gdb.Order("id desc").Limit(10).Find(&rows).Error
				}
				recordLatency(time.Since(opStart))
				if err != nil {
					recordErr(err)
				}
			}
		}(r)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(deadline):
		t.Fatalf("卡死:%d 写者+%d 读者并发读写 %v 未完成", writers, readers, deadline)
	}
	elapsed := time.Since(start)

	// 断言 1:零锁错误
	if n := lockedCnt.Load(); n != 0 {
		t.Errorf("出现 %d 次 database is locked/busy —— 单连接池+busy_timeout 未兜住", n)
	}
	// 其它错误也不许有
	if n := otherErrs.Load(); n != 0 {
		t.Errorf("出现 %d 次非锁错误,首个: %v", n, firstErr.Load())
	}
	// 断言 2:写入全部落库
	var total int64
	if err := gdb.Model(&stressRec{}).Count(&total).Error; err != nil {
		t.Fatalf("终检 count: %v", err)
	}
	if want := int64(writers * opsEach); total != want {
		t.Errorf("落库行数 %d != 期望 %d(有写入丢失)", total, want)
	}
	// 断言 3:单操作延迟有界(不拖垮 recoder.go 的 2s 媒体路径超时)
	maxLat := time.Duration(maxLatNs.Load())
	if maxLat > maxOpLatency {
		t.Errorf("单操作最大延迟 %v 超过 %v 上界 —— 会拖垮 recoder.go 媒体路径 2s 超时", maxLat, maxOpLatency)
	}
	t.Logf("压测通过:%d ops 总耗时 %v(%.0f ops/s),单操作最大延迟 %v,锁错误 0",
		(writers+readers)*opsEach, elapsed.Round(time.Millisecond),
		float64((writers+readers)*opsEach)/elapsed.Seconds(), maxLat.Round(time.Microsecond))
}
