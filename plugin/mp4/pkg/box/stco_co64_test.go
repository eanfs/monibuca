package box

import (
	"bytes"
	"testing"
)

// 小偏移:保持 stco(32 位),往返读写一致。
func TestSTCOSmallOffsetsRoundtrip(t *testing.T) {
	entries := []uint64{100, 2048, 0xFFFFFFFF}
	b := CreateSTCOBox(entries)
	if b.typ != TypeSTCO {
		t.Fatalf("expected stco type, got %s", b.typ)
	}

	var buf bytes.Buffer
	n, err := WriteTo(&buf, b)
	if err != nil {
		t.Fatalf("write stco: %v", err)
	}
	if n != int64(b.Size()) {
		t.Fatalf("written %d != declared size %d", n, b.Size())
	}

	parsed, err := ReadFrom(&buf)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	stco, ok := parsed.(*STCOBox)
	if !ok {
		t.Fatalf("expected *STCOBox, got %T", parsed)
	}
	if len(stco.Entries) != len(entries) {
		t.Fatalf("entry count %d != %d", len(stco.Entries), len(entries))
	}
	for i, e := range entries {
		if stco.Entries[i] != e {
			t.Fatalf("entry[%d] = %d, want %d", i, stco.Entries[i], e)
		}
	}
}

// 大偏移(>4GB):CreateSTCOBox 自动升级为 co64,64 位偏移无截断,
// 且每个 entry 独立写出(回归保护:旧实现所有 entry 复用同一缓冲、
// entry count 多写 4 字节)。
func TestSTCOAutoUpgradeToCO64(t *testing.T) {
	entries := []uint64{0x100000000, 0x123456789A, 0xFFFFFFFFFF}
	b := CreateSTCOBox(entries)
	if b.typ != TypeCO64 {
		t.Fatalf("expected auto-upgrade to co64, got %s", b.typ)
	}
	wantSize := uint64(FullBoxLen + 4 + len(entries)*8)
	if b.Size() != wantSize {
		t.Fatalf("size %d, want %d", b.Size(), wantSize)
	}

	var buf bytes.Buffer
	n, err := WriteTo(&buf, b)
	if err != nil {
		t.Fatalf("write co64: %v", err)
	}
	if n != int64(b.Size()) {
		t.Fatalf("written %d != declared size %d", n, b.Size())
	}

	parsed, err := ReadFrom(&buf)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	co64, ok := parsed.(*CO64Box)
	if !ok {
		t.Fatalf("expected *CO64Box, got %T", parsed)
	}
	if len(co64.Entries) != len(entries) {
		t.Fatalf("entry count %d != %d", len(co64.Entries), len(entries))
	}
	for i, e := range entries {
		if co64.Entries[i] != e {
			t.Fatalf("entry[%d] = %#x, want %#x", i, co64.Entries[i], e)
		}
	}
}
