package mp4

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 默认配置必须保持录制现有行为：写索引、1s 落盘。
func TestIndexConfigDefaults(t *testing.T) {
	cfg := GetIndexConfig()
	if !cfg.Enable {
		t.Fatal("index must be enabled by default")
	}
	if cfg.FlushInterval != time.Second {
		t.Fatalf("flush interval = %v, want 1s", cfg.FlushInterval)
	}
}

// SetIndexConfig 注入后，新建的写入器必须使用配置的落盘间隔；
// <=0 回退默认值，避免关闭节流导致逐帧 fsync。
func TestIndexConfigWiredIntoWriter(t *testing.T) {
	t.Cleanup(func() { SetIndexConfig(defaultIndexConfig) })

	SetIndexConfig(IndexConfig{Enable: false, FlushInterval: 0})
	cfg := GetIndexConfig()
	if cfg.Enable || cfg.FlushInterval != time.Second {
		t.Fatalf("config = %+v, want enable=false interval=1s", cfg)
	}

	SetIndexConfig(IndexConfig{Enable: true, FlushInterval: 250 * time.Millisecond})
	path := filepath.Join(t.TempDir(), "test.m7sidx")
	w, err := CreateIndex(path, IndexHeader{})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if w.flushInterval != 250*time.Millisecond {
		t.Fatalf("writer flush interval = %v, want 250ms", w.flushInterval)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("index file missing: %v", err)
	}
}
