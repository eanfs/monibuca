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
		// 纯文件路径 DSN 注入务实默认(ncruces _pragma 语法):
		//   WAL          — 读写不互斥,降低录制/上传/API 并发下的锁与 fsync 争抢
		//   busy_timeout — 偶发锁竞争时短暂等待而非立即 "database is locked"
		//   synchronous=NORMAL — WAL 下的推荐档,COMMIT 不强制 fsync 主库
		// 用户显式带参数(? / file:)或内存库的 DSN 保持原样。
		if dsn != ":memory:" && !strings.Contains(dsn, "?") && !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + dsn +
				"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
		}
		return gormlite.Open(dsn)
	}
}
