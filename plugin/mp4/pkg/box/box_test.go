package box

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"testing"
)

// TestIsNil 验证 isNil 对纯 nil 接口、typed-nil 指针、正常 box 的判定。
func TestIsNil(t *testing.T) {
	var pureNil IBox
	if !isNil(pureNil) {
		t.Error("纯 nil 接口应判为 nil")
	}
	var typedNil *ContainerBox
	if !isNil(typedNil) {
		t.Error("typed-nil *ContainerBox 应判为 nil")
	}
	if isNil(CreateContainerBox(TypeFREE)) {
		t.Error("正常 box 不应判为 nil")
	}
}

// TestCreateContainerBox_SkipsNilChildren 验证 CreateContainerBox 跳过 nil 子 box
// 且不 panic（旧实现对纯 nil 接口调 reflect.Value.IsNil() 会 panic）。
func TestCreateContainerBox_SkipsNilChildren(t *testing.T) {
	var pureNil IBox
	var typedNil *ContainerBox
	real := CreateContainerBox(TypeFREE)

	c := CreateContainerBox(TypeMOOV, pureNil, typedNil, real)
	if len(c.Children) != 1 {
		t.Fatalf("应只保留 1 个有效子 box，实际 %d", len(c.Children))
	}
}

// TestWriteTo_SkipsNil 验证 WriteTo 跳过 nil 子 box 不 panic。
func TestWriteTo_SkipsNil(t *testing.T) {
	var pureNil IBox
	var typedNil *ContainerBox
	var buf bytes.Buffer

	n, err := WriteTo(&buf, pureNil, typedNil)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != 0 {
		t.Fatalf("全 nil 子 box 应写 0 字节，实际 %d", n)
	}
}

// allocDelta 返回 fn 执行期间的总分配字节数。用来断言"尺寸字段再离谱也不会
// 触发 GB 级分配"——AllocsPerRun 只数次数，数不出 4.29GB 的那一次。
func allocDelta(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// boxHeader 造一个 8 字节 box 头。
func boxHeader(size uint32, typ string) []byte {
	b := make([]byte, BasicBoxLen)
	binary.BigEndian.PutUint32(b[:4], size)
	copy(b[4:], typ)
	return b
}

// largeBoxHeader 造一个 size==1 的 largesize 形式 box 头(16 字节)。
func largeBoxHeader(large uint64, typ string) []byte {
	b := append(boxHeader(1, typ), make([]byte, 8)...)
	binary.BigEndian.PutUint64(b[8:], large)
	return b
}

// 尺寸字段来自不可信文件：size<8 时 make(size-8) 会在 uint32 下溢成 ~4.29GB，
// largesize<16 时在 uint64 下溢成 ~2^64 字节，size==8 的 FullBox 会在 payload[0]
// 越界 panic。三者都必须变成"返回错误"，且不做大分配、不 panic。
func TestReadFromRejectsInvalidSize(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"size_zero", boxHeader(0, "stsd")},
		{"size_four", boxHeader(4, "stsd")},
		{"size_seven", boxHeader(7, "free")},
		// FullBox 负载 0 字节：version/flags 取不到。
		{"size_eight_full_box", boxHeader(8, "stsd")},
		// largesize(8) 连头都不够。
		{"largesize_eight", largeBoxHeader(8, "stsd")},
		{"largesize_zero", largeBoxHeader(0, "stsd")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ReadFrom panicked: %v", r)
				}
			}()
			var got IBox
			var err error
			alloc := allocDelta(func() { got, err = ReadFrom(bytes.NewReader(c.data)) })
			if err == nil {
				t.Fatalf("expected error, got box %+v", got)
			}
			if alloc > 8<<20 {
				t.Fatalf("allocated %d bytes for %d input bytes", alloc, len(c.data))
			}
		})
	}
}

// 顶层 stsd 合法、子 box 声明 size=4：子 box 解析必须报错上抛，不能静默丢弃
// entry（STSDBox.Unmarshal 曾把子 box 错误吞成"0 个 entry"）。
func TestReadFromRejectsNestedUnderflow(t *testing.T) {
	data := make([]byte, 0, 24)
	data = append(data, boxHeader(24, "stsd")...)
	data = append(data, 0, 0, 0, 0) // version/flags
	data = append(data, 0, 0, 0, 1) // entry_count = 1
	data = append(data, boxHeader(4, "free")...)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ReadFrom panicked: %v", r)
		}
	}()
	var got IBox
	var err error
	alloc := allocDelta(func() { got, err = ReadFrom(bytes.NewReader(data)) })
	if err == nil {
		t.Fatalf("expected error for nested size=4, got box %+v", got)
	}
	if alloc > 8<<20 {
		t.Fatalf("allocated %d bytes for %d input bytes", alloc, len(data))
	}
}

// 边界不能误伤合法尺寸：size 恰好 16 的 stsd 与 largesize 恰好 24 的 stsd
// 都必须正常解析。
func TestReadFromAcceptsBoundarySTSD(t *testing.T) {
	regular := append(boxHeader(16, "stsd"), make([]byte, 8)...)
	large := append(largeBoxHeader(24, "stsd"), make([]byte, 8)...)

	for name, data := range map[string][]byte{"size16": regular, "largesize24": large} {
		parsed, err := ReadFrom(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		stsd, ok := parsed.(*STSDBox)
		if !ok {
			t.Fatalf("%s: parsed as %T, want *STSDBox", name, parsed)
		}
		if len(stsd.Entries) != 0 {
			t.Fatalf("%s: entries = %d, want 0", name, len(stsd.Entries))
		}
	}
}

// 合法 stsd 的解析回归：子 box 错误上抛不能误伤正常文件。
func TestReadFromAcceptsValidSTSD(t *testing.T) {
	entry := CreateVisualSampleEntry(TypeAVC1, 320, 240, CreateDataBox(TypeAVCC, []byte{1, 2, 3}))
	var buf bytes.Buffer
	if _, err := WriteTo(&buf, CreateSTSDBox(entry)); err != nil {
		t.Fatalf("serialize stsd: %v", err)
	}
	parsed, err := ReadFrom(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	stsd, ok := parsed.(*STSDBox)
	if !ok {
		t.Fatalf("parsed as %T, want *STSDBox", parsed)
	}
	if len(stsd.Entries) != 1 || stsd.Entries[0].Type() != TypeAVC1 {
		t.Fatalf("entries = %+v, want one avc1", stsd.Entries)
	}
}
