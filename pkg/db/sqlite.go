//go:build sqlite

package db

import (
	"strings"

	//"github.com/glebarez/sqlite"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
)

func init() {
	Factory["sqlite"] = func(dsn string) gorm.Dialector {
		return gormlite.Open(buildSQLiteDSN(dsn))
	}
}

// buildSQLiteDSN 为纯文件路径 DSN 注入务实默认(ncruces _pragma 语法)。
//
// 关键约束:**不注入 journal_mode(WAL)**。
//   - ncruces/go-sqlite3 的 WAL 依赖 -shm 共享内存索引,走 mmap,行为强依赖
//     内核页大小。64K 页(PAGE_SIZE=65536)的 ARM 内核(Kylin V10 SP3 /
//     openEuler 20.03 SP3,kernel 4.19)上 WAL-index mmap 不兼容 → DB 初始化
//     即报 "sqlite3: disk I/O error" 直接 crashloop,且删库重建无效(非文件
//     损坏,是页大小不兼容)。4K 页 x86(oe24.03/kernel 6.6)不受影响。
//   - 并发写锁("database is locked")雪崩由 server.go 的 MaxOpenConns(1)
//     单连接池串行化根治,不依赖 WAL;busy_timeout 兜底偶发撞锁。
//   - 非 WAL(rollback journal)模式下 synchronous=NORMAL 断电可能损坏整库,
//     故不注入 synchronous,保留默认 FULL。
//
// 所以所有架构(4K x86 / 64K ARM)统一走非 WAL + 单连接池。
// 用户显式带参数(? / file:)或内存库的 DSN 保持原样(平台自负)。
func buildSQLiteDSN(dsn string) string {
	if dsn == ":memory:" || strings.Contains(dsn, "?") || strings.HasPrefix(dsn, "file:") {
		return dsn
	}
	return "file:" + dsn + "?_pragma=busy_timeout(5000)"
}
