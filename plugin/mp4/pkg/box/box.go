package box

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"unsafe"

	"github.com/langhuihui/gomem"
)

type (
	BoxType [4]byte

	BoxHeader interface {
		Type() BoxType
		HeaderSize() uint32
		Size() uint64
		Header() BoxHeader
		HeaderWriteTo(w io.Writer) (n int64, err error)
	}

	IBox interface {
		BoxHeader
		io.WriterTo
		Unmarshal(buf []byte) (IBox, error)
	}

	// 基础Box结构，实现通用字段
	BaseBox struct {
		typ  BoxType
		size uint32
	}
	DataBox struct {
		BaseBox
		Data []byte
	}
	MemoryBox struct {
		BaseBox
		Data gomem.Memory
	}
	BigBox struct {
		BaseBox
		size uint64
	}

	FullBox struct {
		BaseBox
		Version uint8
		Flags   [3]byte
	}
	ContainerBox struct {
		BaseBox
		Children []IBox
	}
)

func CreateBaseBox(typ BoxType, size uint64) IBox {
	if size > 0xFFFFFFFF {
		return &BigBox{
			BaseBox: BaseBox{
				typ:  typ,
				size: 1,
			},
			size: size + 8,
		}
	}

	return &BaseBox{
		typ:  typ,
		size: uint32(size),
	}
}

func CreateDataBox(typ BoxType, data []byte) *DataBox {
	return &DataBox{
		BaseBox: BaseBox{
			typ:  typ,
			size: uint32(len(data)) + BasicBoxLen,
		},
		Data: data,
	}
}

func CreateMemoryBox(typ BoxType, mem gomem.Memory) *MemoryBox {
	return &MemoryBox{
		BaseBox: BaseBox{
			typ:  typ,
			size: uint32(mem.Size) + BasicBoxLen,
		},
		Data: mem,
	}
}

// isNil 安全判断 IBox 是否为 nil。直接对接口值调 reflect.Value.IsNil() 在
// 「纯 nil 接口」(zero Value)和「值类型」上会 panic;本函数规避之:
// 纯 nil 接口返回 true,可空 Kind(指针/接口/slice 等)的 nil 值返回 true,
// 其余(如值类型 struct)返回 false。
func isNil(i IBox) bool {
	if i == nil {
		return true
	}
	v := reflect.ValueOf(i)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.UnsafePointer, reflect.Interface, reflect.Slice:
		return v.IsNil()
	}
	return false
}

func CreateContainerBox(typ BoxType, children ...IBox) *ContainerBox {
	size := uint32(BasicBoxLen)
	realChildren := make([]IBox, 0, len(children))
	for _, child := range children {
		if isNil(child) {
			continue
		}
		size += uint32(child.Size())
		realChildren = append(realChildren, child)
	}
	return &ContainerBox{
		BaseBox: BaseBox{
			typ:  typ,
			size: size,
		},
		Children: realChildren,
	}
}

func (b *BigBox) HeaderSize() uint32 { return BasicBoxLen + 8 }

func (b *BaseBox) Header() BoxHeader  { return b }
func (b *BaseBox) HeaderSize() uint32 { return BasicBoxLen }
func (b *BaseBox) Size() uint64       { return uint64(b.size) }
func (b *BigBox) Size() uint64        { return uint64(b.size) }
func (b *BaseBox) Type() BoxType      { return b.typ }

func (b *BaseBox) HeaderWriteTo(w io.Writer) (n int64, err error) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], b.size)
	buffers := net.Buffers{tmp[:], b.typ[:]}
	return buffers.WriteTo(w)
}

func (b *BaseBox) WriteTo(w io.Writer) (n int64, err error) {
	return
}

func (b *ContainerBox) WriteTo(w io.Writer) (n int64, err error) {
	return WriteTo(w, b.Children...)
}

func (b *DataBox) WriteTo(w io.Writer) (n int64, err error) {
	_, err = w.Write(b.Data)
	return int64(len(b.Data)), err
}

func (b *BaseBox) Unmarshal(buf []byte) (IBox, error) {
	return b, nil
}

func (b *MemoryBox) WriteTo(w io.Writer) (n int64, err error) {
	return b.Data.WriteTo(w)
}

func (b *MemoryBox) Unmarshal(buf []byte) (IBox, error) {
	b.Data.PushOne(buf)
	return b, nil
}

func (b *DataBox) Unmarshal(buf []byte) (IBox, error) {
	b.Data = buf
	return b, nil
}

func (b *FullBox) HeaderWriteTo(w io.Writer) (n int64, err error) {

	var tmp [4]byte

	binary.BigEndian.PutUint32(tmp[:], b.size)
	buffers := net.Buffers{tmp[:], b.typ[:], []byte{b.Version}, b.Flags[:]}
	return buffers.WriteTo(w)
}

func (b *BigBox) HeaderWriteTo(w io.Writer) (n int64, err error) {
	n, err = b.BaseBox.HeaderWriteTo(w)
	if err != nil {
		return
	}
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], b.size)
	_, err = w.Write(tmp[:])
	return n + 8, err
}

func (b *BigBox) Header() BoxHeader   { return b }
func (b *FullBox) Header() BoxHeader  { return b }
func (b *FullBox) HeaderSize() uint32 { return FullBoxLen }

func WriteTo(w io.Writer, box ...IBox) (n int64, err error) {
	var n1, n2 int64
	for _, b := range box {
		if isNil(b) {
			continue
		}
		n1, err = b.HeaderWriteTo(w)
		if err != nil {
			return
		}
		n2, err = b.WriteTo(w)
		if err != nil {
			return
		}
		// 声明尺寸与实际写出不一致会产出结构错位的静默损坏文件,必须报错。
		// n2==0 豁免:mdat/free 等仅覆盖 box header 的写法(payload 已在盘上)。
		if n2 > 0 && n1+n2 != int64(b.Size()) {
			return n, fmt.Errorf("box %s size mismatch: wrote %d, declared %d", b.Type(), n1+n2, b.Size())
		}
		n += n1 + n2
	}
	return

}

func ReadFrom(r io.Reader) (box IBox, err error) {
	var tmp [8]byte
	if _, err = io.ReadFull(r, tmp[:]); err != nil {
		return
	}
	var baseBox BaseBox
	baseBox.size = binary.BigEndian.Uint32(tmp[:4])
	baseBox.typ = BoxType(tmp[4:])
	t, exists := registry[baseBox.typ.Uint32I()]
	if !exists {
		// 未知类型按声明长度丢弃。这是已知取舍：size<8 时 uint32 相减会得到
		// ~4.29GB，CopyN 会请求一次 4.29GB 的"丢弃读取"——它不分配内存，对
		// *os.File 只是空读到结尾，因此这里有意不加校验，避免误伤带未知尾部
		// box(含 size==0 的合法"延伸到文件尾"写法)的历史行为。
		io.CopyN(io.Discard, r, int64(baseBox.size-BasicBoxLen))
		return &baseBox, nil
	}
	b := reflect.New(t.Elem()).Interface().(IBox)
	var payload []byte
	if baseBox.size == 1 {
		if _, err = io.ReadFull(r, tmp[:]); err != nil {
			return
		}
		largeSize := binary.BigEndian.Uint64(tmp[:])
		// largesize 至少要覆盖 size(4)+type(4)+largesize(8)；更小的值会让下面的
		// 相减在 uint64 下回绕成 ~2^64 字节的分配。损坏/伪造文件不能借尺寸字段
		// 决定内存占用。
		if largeSize < BasicBoxLen+8 {
			return nil, fmt.Errorf("box %s: declared largesize %d smaller than header (%d)",
				baseBox.typ.String(), largeSize, BasicBoxLen+8)
		}
		payloadLen := largeSize - BasicBoxLen - 8
		if err = checkDeclaredPayload(baseBox.typ, payloadLen, r); err != nil {
			return nil, err
		}
		// payloadLen 已校验过不超过剩余量/兜底上限，转 int 不会溢出或为负。
		payload = make([]byte, payloadLen)
	} else {
		// 声明长度至少要覆盖 8 字节 box 头；更小的值(size<8)会让 uint32 相减
		// 回绕成 ~4.29GB 的分配。
		if baseBox.size < BasicBoxLen {
			return nil, fmt.Errorf("box %s: declared size %d smaller than header (%d)",
				baseBox.typ.String(), baseBox.size, BasicBoxLen)
		}
		payloadLen := uint64(baseBox.size - BasicBoxLen)
		if err = checkDeclaredPayload(baseBox.typ, payloadLen, r); err != nil {
			return nil, err
		}
		payload = make([]byte, payloadLen)
	}
	_, err = io.ReadFull(r, payload)
	if err != nil {
		return nil, err
	}
	boxHeader := b.Header()
	switch header := boxHeader.(type) {
	case *BaseBox:
		*header = baseBox
		box, err = b.Unmarshal(payload)
	case *FullBox:
		header.BaseBox = baseBox
		// FullBox 的负载至少要有 version(1)+flags(3)；payload 更短时下面两次取值
		// 会越界 panic（典型是声明 size==8 的 stsd 之类）。
		if len(payload) < 4 {
			return nil, fmt.Errorf("box %s: payload %d too short for full box", baseBox.typ.String(), len(payload))
		}
		header.Version = payload[0]
		header.Flags = [3]byte(payload[1:4])
		box, err = b.Unmarshal(payload[4:])
	}
	if err == io.EOF {
		return box, nil
	}
	return
}

// remainingBytes 尽量返回 r 中尚未读取的字节数；第二个返回值为 false 表示无法判断。
// bytes.Reader/strings.Reader/SectionReader 的 Len() 与 *os.File 的 Size()-Seek()
// 都能精确给出。
func remainingBytes(r io.Reader) (int64, bool) {
	switch v := r.(type) {
	case interface{ Len() int }:
		return int64(v.Len()), true
	case interface {
		Stat() (os.FileInfo, error)
		Seek(offset int64, whence int) (int64, error)
	}:
		info, err := v.Stat()
		if err != nil {
			return 0, false
		}
		// 管道/设备/目录的 Size() 没有"剩余字节"的含义（常为 0），误用会把所有
		// 带负载的 box 判成超限，退回兜底上限。
		if !info.Mode().IsRegular() {
			return 0, false
		}
		cur, err := v.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, false
		}
		if cur >= info.Size() {
			return 0, true
		}
		return info.Size() - cur, true
	}
	return 0, false
}

// checkDeclaredPayload 在分配负载缓冲区前校验声明长度，确保"文件里的尺寸字段
// 不能决定内存分配"：
//   - 能拿到剩余字节时，声明超过剩余量直接报错。错误包装 io.ErrUnexpectedEOF，
//     语义仍是"文件被截断"——进行中的录制读到半个 box 时调用方照旧优雅收尾；
//   - 拿不到剩余量时退回绝对上限 maxBoxPayload，只防 panic/OOM，不是业务限制。
func checkDeclaredPayload(typ BoxType, payloadLen uint64, r io.Reader) error {
	if remaining, ok := remainingBytes(r); ok {
		if payloadLen > uint64(remaining) {
			return fmt.Errorf("box %s: declared payload %d exceeds %d bytes remaining: %w",
				typ.String(), payloadLen, remaining, io.ErrUnexpectedEOF)
		}
		return nil
	}
	if payloadLen > maxBoxPayload {
		return fmt.Errorf("box %s: declared payload %d exceeds limit %d",
			typ.String(), payloadLen, uint64(maxBoxPayload))
	}
	return nil
}

func (b BoxType) String() string {
	return string(b[:])
}

func (b BoxType) Uint32() uint32 {
	return binary.BigEndian.Uint32(b[:])
}

func (b BoxType) Uint32I() uint32 {
	return *(*uint32)(unsafe.Pointer(&b[0]))
}

var registry = map[uint32]reflect.Type{}

// RegisterBox 注册box类型
func RegisterBox[T any](typ ...BoxType) {
	var b T
	bt := reflect.TypeOf(b)
	for _, t := range typ {
		registry[t.Uint32I()] = bt
	}
}

const (
	BasicBoxLen = 8  // size(4) + type(4)
	FullBoxLen  = 12 // BasicBoxLen + version(1) + flags(3)

	// maxBoxPayload 是"拿不到 reader 剩余字节"时的兜底上限，只用于防 panic/OOM，
	// 不是业务限制：mdat 等 box 可以有合法的超大尺寸，那类调用方的 reader
	// (*os.File / bytes.Reader) 能精确给出剩余量，走的是精确校验分支。
	maxBoxPayload = 1 << 30
)

func f(s string) BoxType {
	return BoxType([]byte(s))
}

var (
	TypeFTYP = f("ftyp")
	TypeSTYP = f("styp")
	TypeMOOV = f("moov")
	TypeMVHD = f("mvhd")
	TypeTRAK = f("trak")
	TypeTKHD = f("tkhd")
	TypeMDIA = f("mdia")
	TypeMDHD = f("mdhd")
	TypeHDLR = f("hdlr")
	TypeMINF = f("minf")
	TypeSTBL = f("stbl")
	TypeSTSD = f("stsd")
	TypeSTTS = f("stts")
	TypeSTSC = f("stsc")
	TypeSTSZ = f("stsz")
	TypeSTCO = f("stco")
	TypeMDAT = f("mdat")
	TypeFREE = f("free")
	TypeUUID = f("uuid")

	TypeVMHD = f("vmhd")
	TypeSMHD = f("smhd")
	TypeHMHD = f("hmhd")
	TypeNMHD = f("nmhd")
	TypeCTTS = f("ctts")
	TypeCO64 = f("co64")
	TypePSSH = f("pssh")

	TypeSTSS = f("stss")
	TypeENCV = f("encv")
	TypeSINF = f("sinf")
	TypeFRMA = f("frma")
	TypeSCHI = f("schi")
	TypeTENC = f("tenc")
	TypeAVC1 = f("avc1")
	TypeHVC1 = f("hvc1")
	TypeHEV1 = f("hev1")
	TypeENCA = f("enca")
	TypeMP4A = f("mp4a")
	TypeULAW = f("ulaw")
	TypeALAW = f("alaw")
	TypeDOPS = f("dOps")
	TypeOPUS = f("opus")
	TypeAVCC = f("avcC")
	TypeHVCC = f("hvcC")
	TypeESDS = f("esds")
	TypeEDTS = f("edts")
	TypeELST = f("elst")
	TypeMVEX = f("mvex")
	TypeMEHD = f("mehd")
	TypeMOOF = f("moof")
	TypeMFHD = f("mfhd")
	TypeTRAF = f("traf")
	TypeTFHD = f("tfhd")
	TypeTFDT = f("tfdt")
	TypeTRUN = f("trun")
	TypeSDTP = f("sdtp")
	TypeSENC = f("senc")
	TypeSAIZ = f("saiz")
	TypeSAIO = f("saio")
	TypeSGPD = f("sgpd")
	TypeWAVE = f("wave")
	TypeMSDH = f("msdh")
	TypeMSIX = f("msix")
	TypeISOM = f("isom")
	TypeISO2 = f("iso2")
	TypeISO3 = f("iso3")
	TypeISO4 = f("iso4")
	TypeISO5 = f("iso5")
	TypeISO6 = f("iso6")
	TypeMP41 = f("mp41")
	TypeMP42 = f("mp42")
	TypeDASH = f("dash")
	TypeMFRA = f("mfra")
	TypeMFRO = f("mfro")
	TypeTREX = f("trex")
	TypeTFRA = f("tfra")
	TypeSIDX = f("sidx")
	TypeDINF = f("dinf")
	TypeDREF = f("dref")
	TypeVIDE = f("vide")
	TypeSOUN = f("soun")
	TypeMETA = f("meta")
	TypeAUXV = f("auxv")
	TypeHINT = f("hint")
	TypeUDTA = f("udta")

	// Common metadata box types
	TypeTITL      = f("©nam") // Title
	TypeART       = f("©ART") // Artist/Author
	TypeALB       = f("©alb") // Album
	TypeDAY       = f("©day") // Date/Year
	TypeCMT       = f("©cmt") // Comment/Description
	TypeGEN       = f("©gen") // Genre
	TypeCPRT      = f("cprt") // Copyright
	TypeENCO      = f("©too") // Encoder/Tool
	TypeWRT       = f("©wrt") // Writer/Composer
	TypePRD       = f("©prd") // Producer
	TypePRF       = f("©prf") // Performer
	TypeGRP       = f("©grp") // Grouping
	TypeLYR       = f("©lyr") // Lyrics
	TypeKEYW      = f("keyw") // Keywords
	TypeLOCI      = f("loci") // Location Information
	TypeRTNG      = f("rtng") // Rating
	TypeMETA_CUST = f("----") // Custom metadata (iTunes-style)
)

//	aligned(8) class Box (unsigned int(32) boxtype, optional unsigned int(8)[16] extended_type) {
//	    unsigned int(32) size;
//	    unsigned int(32) type = boxtype;
//	    if (size==1) {
//	       unsigned int(64) largesize;
//	    } else if (size==0) {
//	       // box extends to end of file
//	    }
//	    if (boxtype=='uuid') {
//	    unsigned int(8)[16] usertype = extended_type;
//	 }
//	}

// aligned(8) class FullBox(unsigned int(32) boxtype, unsigned int(8) v, bit(24) f) extends Box(boxtype) {
//     unsigned int(8) version = v;
//     bit(24) flags = f;
// }

type TimeToSampleEntry struct {
	SampleCount uint32
	SampleDelta uint32
}

type CompositionTimeToSampleEntry struct {
	SampleCount  uint32
	SampleOffset int32
}

type SampleToChunkEntry struct {
	FirstChunk             uint32
	SamplesPerChunk        uint32
	SampleDescriptionIndex uint32
}

func ConvertUnixTimeToISO14496(unixTime uint64) uint64 {
	return unixTime + 0x7C25B080
}
