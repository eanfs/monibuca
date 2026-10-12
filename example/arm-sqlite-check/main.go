//go:build sqlite

// arm-sqlite-check: 最小复现器,验证 ncruces/go-sqlite3 的 WAL 模式在大页
// (>4K,尤其 64K ARM 内核)上是否 crashloop,以及本仓 no-WAL 修复是否可用。
//
// 交叉编译到 64K 页 ARM(Kylin V10 SP3 / openEuler 20.03 SP3,kernel 4.19):
//   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags sqlite -o arm-sqlite-check ./example/arm-sqlite-check
//
// 期望结果(64K 页机):OLD WAL = false(disk I/O error),NEW fix = true。
// 期望结果(4K 页机):两者皆 true(仅证明修复无回退副作用)。
package main

import (
	"fmt"
	"os"
	"syscall"

	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type kv struct {
	ID   uint `gorm:"primaryKey"`
	Name string
}

// try 打开 dsn、建表、写一行,任一步失败即返回 false 并打印错误。
func try(label, dsn string) bool {
	fmt.Printf("\n[%s]\n  DSN = %s\n", label, dsn)
	db, err := gorm.Open(gormlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		fmt.Printf("  ✗ open: %v\n", err)
		return false
	}
	if err := db.AutoMigrate(&kv{}); err != nil {
		fmt.Printf("  ✗ migrate: %v\n", err)
		return false
	}
	if err := db.Create(&kv{Name: "hello"}).Error; err != nil {
		fmt.Printf("  ✗ write: %v\n", err)
		return false
	}
	fmt.Printf("  ✓ ok\n")
	return true
}

func main() {
	pg := syscall.Getpagesize()
	fmt.Printf("PAGE_SIZE = %d\n", pg)

	for _, f := range []string{"/tmp/armcheck_wal.db", "/tmp/armcheck_now.db"} {
		os.Remove(f)
		os.Remove(f + "-wal")
		os.Remove(f + "-shm")
	}

	// OLD:v5.3.x(dd2b96ec)对纯路径 DSN 注入的默认 —— 含 journal_mode(WAL)。
	oldWAL := try("OLD  v5.3.x (WAL)", "file:/tmp/armcheck_wal.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	// NEW:本仓修复后 buildSQLiteDSN 注入的默认 —— 无 WAL、无 synchronous。
	newFix := try("NEW  fix  (no-WAL)", "file:/tmp/armcheck_now.db?_pragma=busy_timeout(5000)")

	fmt.Printf("\n==== RESULT (PAGE_SIZE=%d) ====\n", pg)
	fmt.Printf("OLD WAL : %v\n", oldWAL)
	fmt.Printf("NEW fix : %v\n", newFix)

	switch {
	case !newFix:
		fmt.Println("结论:❌ 修复本身打不开库 —— 严重,任何页大小都不该失败")
		os.Exit(2)
	case pg > 4096 && !oldWAL:
		fmt.Printf("结论:✅ 复现成功 —— %dK 页上 WAL 崩、no-WAL 修复正常\n", pg/1024)
	case pg > 4096 && oldWAL:
		fmt.Printf("结论:⚠ 该 %dK 页内核/驱动组合下 WAL 未崩 —— 此机不复现,换 64K 内核再验\n", pg/1024)
	default:
		fmt.Println("结论:✅ 4K 页,两者皆通 —— 修复无回退副作用(WAL 崩溃需 64K 页机才能验)")
	}
}
