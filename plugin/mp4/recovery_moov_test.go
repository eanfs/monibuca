package plugin_mp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// moovTestBox 序列化一个常规 32-bit box。
func moovTestBox(typ string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(body)))
	copy(out[4:8], typ)
	return append(out, body...)
}

// moovTestBoxWithSize 序列化一个 32-bit box，但 size 字段可以故意写成
// 与实际负载不符的值（模拟写到一半掉电：头部先落盘，负载没写完）。
func moovTestBoxWithSize(typ string, declaredSize uint32, body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out[:4], declaredSize)
	copy(out[4:8], typ)
	return append(out, body...)
}

// moovTestWrite 把若干 box 拼接后写到临时文件，返回路径与字节。
func moovTestWrite(t *testing.T, boxes ...[]byte) (string, []byte) {
	t.Helper()
	data := bytes.Join(boxes, nil)
	path := filepath.Join(t.TempDir(), "media.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	return path, data
}

// moovTestCompleteMoov 造一个内容完整（含 mvhd）的 moov。
func moovTestCompleteMoov() []byte {
	return moovTestBox("moov", bytes.Join([][]byte{
		moovTestBox("mvhd", make([]byte, 100)),
		moovTestBox("trak", []byte{0, 0, 0, 0}),
	}, nil))
}

// Important 3：trailer 把 moov 写到文件头的中途掉电时，size 字段可能已落盘
// 而 moov 内容没写完。只看 box 头会把这种半成品判成“已收尾”，随后入队并删掉
// sidecar——恢复侧再也不会重建它。声明尺寸越过文件尾必须判为未收尾。
func TestHasTopLevelMoovRejectsSizeBeyondEOF(t *testing.T) {
	ftyp := moovTestBox("ftyp", []byte("isom"))
	// moov 声明 4096 字节，实际只写了 24 字节负载。
	partialMoov := moovTestBoxWithSize("moov", 4096, make([]byte, 24))
	path, _ := moovTestWrite(t, ftyp, partialMoov)

	if hasTopLevelMoov(path) {
		t.Fatal("moov whose declared size exceeds the file must not count as finalized")
	}
}

// Important 3：moov 完整落在文件内但内部还没有 mvhd（moov 子 box 尚未写全），
// 同样不能算已收尾。
func TestHasTopLevelMoovRejectsMoovWithoutMvhd(t *testing.T) {
	ftyp := moovTestBox("ftyp", []byte("isom"))
	// moov 内只有一个空的 free 子 box：结构自洽但没有 mvhd。
	moov := moovTestBox("moov", moovTestBox("free", make([]byte, 16)))
	path, _ := moovTestWrite(t, ftyp, moov, moovTestBox("mdat", make([]byte, 8)))

	if hasTopLevelMoov(path) {
		t.Fatal("moov without mvhd must not count as finalized")
	}
}

// 完整的 [ftyp][moov(mvhd)][mdat] 仍判为已收尾——加强校验不能误伤正常文件。
func TestHasTopLevelMoovAcceptsCompleteMoov(t *testing.T) {
	ftyp := moovTestBox("ftyp", []byte("isom"))
	path, _ := moovTestWrite(t, ftyp, moovTestCompleteMoov(), moovTestBox("mdat", make([]byte, 8)))

	if !hasTopLevelMoov(path) {
		t.Fatal("complete moov with mvhd should count as finalized")
	}
}

// moov 用 64-bit largesize 时，越界判定同样要生效。
func TestHasTopLevelMoovRejectsLargeSizeBeyondEOF(t *testing.T) {
	ftyp := moovTestBox("ftyp", []byte("isom"))
	large := make([]byte, 16, 16+16)
	binary.BigEndian.PutUint32(large[:4], 1)
	copy(large[4:8], "moov")
	binary.BigEndian.PutUint64(large[8:16], 1<<20) // 声明 1MB
	path, _ := moovTestWrite(t, ftyp, append(large, make([]byte, 16)...))

	if hasTopLevelMoov(path) {
		t.Fatal("largesize moov beyond EOF must not count as finalized")
	}
}
