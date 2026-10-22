package plugin_mp4

// 半成品 progressive-MP4 临时文件（进程被杀/断电后留下的 s3writer_*.tmp）的
// 反向重建。
//
// 这类文件没有 moov，也没有样本表，但载荷本身是完整的 AVCC 码流：
//
//	ftyp(32) | free(8) | mdat header(8, size 占位) | payload(48 起)
//
// 载荷里视频样本是 [4 字节大端长度][NAL] 重复（一个样本可能含多个 slice），
// 音频样本是裸编码帧（无长度前缀），扫描时表现为“断点/空洞”。
// 扫描视频 NAL 重建样本表与 moov，再按 AVCC 原样拷贝视频样本即可得到可播放
// 的 progressive MP4。算法与现场实测结论见 testdata/recovery/README.md。
//
// 边界与限制：
//   - 只处理本仓 progressive 录制器产出的布局；fragmented MP4 布局不同，直接拒绝。
//   - 源文件只以 O_RDONLY 打开，绝不改写。
//   - 时间戳在 tmp 中不存在，只能按 SPS VUI 帧率（或调用方指定帧率）均匀外推。
//   - 音频是启发式恢复（定长 G.711），无法可靠恢复时降级为视频-only 并告警。

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	h264parser "github.com/deepch/vdk/codec/h264parser"
	h265parser "github.com/deepch/vdk/codec/h265parser"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

const (
	// legacyPayloadOffset 是半成品 tmp 中样本载荷的固定起点：
	// 32(ftyp) + 8(free) + 8(mdat 占位头)。
	legacyPayloadOffset int64 = 48

	legacyMaxNALSize      = 8 << 20 // 单个 NAL 的上限（扫描重同步阈值，与 spike 探针一致）
	legacyMaxParamSetSize = 1 << 20

	// 音频启发式恢复的限制。G.711 8 kHz 的常见帧长是 160 B(20 ms) 或 320 B(40 ms)。
	legacyAudioMinFrame = 160
	legacyAudioMaxFrame = 4096
	// 超大空洞通常是截断/损坏残片，不作为音频样本来源。
	legacyMaxAudioHole = 64 << 10
	legacyAudioRate    = 8000

	legacyDefaultFPS = 25.0
)

// LegacyOptions 控制无索引 tmp 的反向重建。
// OutputPath 为空且 DryRun 为 false 时函数只做分析（等价于 dry-run），不写文件。
type LegacyOptions struct {
	// FPS 为 0 时按 SPS VUI 推断，推断失败回退 25 fps。
	FPS float64
	// VideoOnly 明确放弃音频恢复。
	VideoOnly bool
	// OutputPath 是重建产物路径；会以原子 rename 覆盖同名文件。
	OutputPath string
	// DryRun 只扫描/构图并返回报告，不写任何文件。
	DryRun bool
}

// LegacyResult 是一次反向重建的结果。音频字段仅在启发式恢复成功时可信。
type LegacyResult struct {
	OutputPath     string
	VideoFrames    int
	AudioFrames    int
	AudioRecovered bool
	AudioCodec     string
	DurationMs     int64
	Warnings       []string
}

type legacyCodec int

const (
	legacyCodecH264 legacyCodec = iota
	legacyCodecH265
)

type legacySpan struct {
	off  int64
	size int64
}

type legacyNAL struct {
	off  int64 // NAL 数据起点（不含 4 字节长度前缀）
	size int64
}

type legacyAU struct {
	nals     []legacyNAL
	keyFrame bool
	size     int64
}

type legacyHole struct {
	off  int64
	size int64
}

type legacyScan struct {
	codec    legacyCodec
	aus      []legacyAU
	holes    []legacyHole
	sps      []byte
	pps      []byte
	vps      []byte
	warnings []string
}

type legacyAudioFrame struct {
	off  int64
	size int64
}

// RebuildFromMDAT 从半成品 tmp 反向重建可播放的 progressive MP4。
//
// 两遍式处理：先顺序扫描载荷，只记录 AU/空洞的偏移与大小（内存与样本数成正比，
// 不随文件体积增长）；再按样本表写 moov，并逐样本流式拷贝载荷。
func RebuildFromMDAT(mediaPath string, opts LegacyOptions) (LegacyResult, error) {
	res := LegacyResult{}
	if mediaPath == "" {
		return res, errors.New("mp4 legacy recovery: empty media path")
	}
	if !opts.DryRun && opts.OutputPath != "" {
		same, err := legacySamePath(mediaPath, opts.OutputPath)
		if err != nil {
			return res, err
		}
		if same {
			return res, errors.New("mp4 legacy recovery: refusing to overwrite the source tmp")
		}
	}

	// 只读打开：恢复过程绝不修改现场 tmp。
	f, err := os.Open(mediaPath)
	if err != nil {
		return res, fmt.Errorf("mp4 legacy recovery: open media: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return res, fmt.Errorf("mp4 legacy recovery: stat media: %w", err)
	}
	if info.Size() <= legacyPayloadOffset {
		return res, fmt.Errorf("mp4 legacy recovery: file too small (%d B)", info.Size())
	}
	if err = legacyValidateHeader(f); err != nil {
		return res, err
	}

	scan, err := legacyScanPayload(f, info.Size())
	if err != nil {
		return res, err
	}
	res.Warnings = append(res.Warnings, scan.warnings...)
	if len(scan.aus) == 0 {
		return res, errors.New("mp4 legacy recovery: no video access unit found in payload")
	}

	stsd, width, height, parserFPS, err := legacyBuildVideoSTSD(scan)
	if err != nil {
		return res, err
	}
	fps, fpsWarn := legacyResolveFPS(scan, opts.FPS, parserFPS)
	if fpsWarn != "" {
		res.Warnings = append(res.Warnings, fpsWarn)
	}

	var audioFrames []legacyAudioFrame
	var audioCodec string
	if opts.VideoOnly {
		res.Warnings = append(res.Warnings, "audio skipped by request (video-only)")
	} else {
		audioFrames, audioCodec, res.Warnings = legacyRecoverAudio(f, scan.holes, res.Warnings)
	}

	header := mp4pkg.IndexHeader{Tracks: []mp4pkg.IndexTrack{{
		ID:     1,
		FourCC: legacyFourCC(scan.codec),
		Width:  width,
		Height: height,
		STSD:   stsd,
	}}}
	if len(audioFrames) > 0 {
		if audioSTSD, err := legacyBuildAudioSTSD(audioCodec); err != nil {
			res.Warnings = append(res.Warnings, "audio track skipped: "+err.Error())
			audioFrames, audioCodec = nil, ""
		} else {
			frameMs := int(math.Round(float64(audioFrames[0].size) * 1000.0 / legacyAudioRate))
			if frameMs < 1 {
				frameMs = 1
			}
			header.Tracks = append(header.Tracks, mp4pkg.IndexTrack{
				ID:         2,
				FourCC:     legacyAudioFourCC(audioCodec),
				SampleRate: legacyAudioRate,
				Channels:   1,
				STSD:       audioSTSD,
			})
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("audio recovered heuristically as %s %d Hz mono (%d ms/frame); "+
					"timestamps are estimated and long recordings may drift against video",
					audioCodec, legacyAudioRate, frameMs))
		}
	}

	records, parts := legacyBuildRecords(scan, fps, audioFrames)
	m := mp4pkg.NewMuxer(0)
	if err = m.RestoreFromIndex(header, records, -1); err != nil {
		return res, fmt.Errorf("mp4 legacy recovery: build sample table: %w", err)
	}
	var moov bytes.Buffer
	if err = m.WriteMoovOnly(&moov); err != nil {
		return res, fmt.Errorf("mp4 legacy recovery: write moov: %w", err)
	}
	// 自检：mdat 声明的载荷长度必须与实际要拷贝的样本字节数一致。
	// 不一致说明样本表和载荷构建有 bug，宁可失败也不产出偏移错位的文件。
	var expected int64
	for _, part := range parts {
		expected += part.size
	}
	if start, size, ok := m.RestorePayloadRange(); !ok || start != 0 || size != expected {
		return res, fmt.Errorf("mp4 legacy recovery: payload size mismatch (moov=%d, samples=%d)", size, expected)
	}

	res.VideoFrames = len(scan.aus)
	res.AudioFrames = len(audioFrames)
	res.AudioRecovered = len(audioFrames) > 0
	res.AudioCodec = audioCodec
	res.DurationMs = int64(math.Round(float64(res.VideoFrames) * 1000.0 / fps))
	if res.AudioRecovered {
		audioMs := int64(math.Round(float64(res.AudioFrames) * 1000.0 * float64(audioFrames[0].size) / legacyAudioRate))
		if diff := audioMs - res.DurationMs; diff > 1000 || diff < -1000 {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("audio duration (%d ms) differs from video duration (%d ms); A/V drift is expected",
					audioMs, res.DurationMs))
		}
	}

	if opts.DryRun || opts.OutputPath == "" {
		return res, nil
	}
	if err = legacyWriteOutput(f, opts.OutputPath, moov.Bytes(), parts); err != nil {
		return res, err
	}
	res.OutputPath = opts.OutputPath
	return res, nil
}

// ---------------------------------------------------------------------------
// 头部校验与扫描
// ---------------------------------------------------------------------------

func legacyValidateHeader(f *os.File) error {
	var h [legacyPayloadOffset]byte
	if _, err := f.ReadAt(h[:], 0); err != nil {
		return fmt.Errorf("mp4 legacy recovery: read header: %w", err)
	}
	if string(h[4:8]) != "ftyp" {
		return errors.New(`mp4 legacy recovery: not a progressive MP4 temp file (missing ftyp at 0)`)
	}
	if string(h[36:40]) != "free" {
		return errors.New("mp4 legacy recovery: unexpected layout (missing free box at 32)")
	}
	if string(h[44:48]) != "mdat" {
		return errors.New("mp4 legacy recovery: unexpected layout (missing mdat at 40)")
	}
	if size := binary.BigEndian.Uint32(h[40:44]); size != 0 && size != 8 {
		return fmt.Errorf("mp4 legacy recovery: mdat is not the unfinalized placeholder (size=%d)", size)
	}
	return nil
}

// legacyDetectCodec 用第一个 NAL 判定 H.264/H.265。H.264 的类型上限是 23，
// 因此 HEVC 的 VPS（首字节 0x40）不会被误判成 H.264。
func legacyDetectCodec(f *os.File, fileSize int64) (legacyCodec, error) {
	var lens [4]byte
	if _, err := f.ReadAt(lens[:], legacyPayloadOffset); err != nil {
		return 0, fmt.Errorf("mp4 legacy recovery: read first NAL length: %w", err)
	}
	n := int64(binary.BigEndian.Uint32(lens[:]))
	if n <= 0 || n > legacyMaxNALSize || legacyPayloadOffset+4+n > fileSize {
		return 0, errors.New("mp4 legacy recovery: payload does not start with an AVCC length-prefixed NAL")
	}
	var head [2]byte
	read := n
	if read > 2 {
		read = 2
	}
	if _, err := f.ReadAt(head[:read], legacyPayloadOffset+4); err != nil {
		return 0, fmt.Errorf("mp4 legacy recovery: read first NAL header: %w", err)
	}
	if _, ok := legacyNALType(legacyCodecH264, head[:read]); ok {
		return legacyCodecH264, nil
	}
	if _, ok := legacyNALType(legacyCodecH265, head[:read]); ok {
		return legacyCodecH265, nil
	}
	return 0, errors.New("mp4 legacy recovery: cannot detect codec (neither H.264 nor H.265 AVCC)")
}

func legacyNALType(codec legacyCodec, head []byte) (int, bool) {
	if codec == legacyCodecH264 {
		if len(head) < 1 || head[0]&0x80 != 0 {
			return 0, false
		}
		t := int(head[0] & 0x1F)
		return t, t >= 1 && t <= 23
	}
	if len(head) < 2 || head[0]&0x80 != 0 {
		return 0, false
	}
	t := int((head[0] >> 1) & 0x3F)
	return t, t <= 40
}

func legacyScanPayload(f *os.File, fileSize int64) (*legacyScan, error) {
	codec, err := legacyDetectCodec(f, fileSize)
	if err != nil {
		return nil, err
	}
	s := &legacyScan{codec: codec}
	g := &legacyGrouper{scan: s, f: f}

	sr := io.NewSectionReader(f, legacyPayloadOffset, fileSize-legacyPayloadOffset)
	br := bufio.NewReaderSize(sr, 1<<20)
	pos := legacyPayloadOffset
	var holeStart int64 = -1

	closeHole := func(end int64) {
		if holeStart >= 0 && end > holeStart {
			s.holes = append(s.holes, legacyHole{off: holeStart, size: end - holeStart})
		}
		holeStart = -1
	}

	for {
		lens, err := br.Peek(4)
		if err != nil {
			// 尾部不足一个长度字段：整段按空洞/垃圾处理。
			if holeStart < 0 && pos < fileSize {
				holeStart = pos
			}
			closeHole(fileSize)
			break
		}
		n := int64(binary.BigEndian.Uint32(lens))
		headLen := int64(1)
		if codec == legacyCodecH265 {
			headLen = 2
		}
		valid := n > 0 && n <= legacyMaxNALSize && pos+4+n <= fileSize && n >= headLen
		var typ int
		if valid {
			head, err := br.Peek(int(4 + headLen))
			if err != nil {
				valid = false
			} else {
				typ, valid = legacyNALType(codec, head[4:])
			}
		}
		if !valid {
			// 逐字节重同步：音频裸帧、截断残片都会走到这里。
			if holeStart < 0 {
				holeStart = pos
			}
			if _, err := br.Discard(1); err != nil {
				closeHole(fileSize)
				break
			}
			pos++
			continue
		}

		closeHole(pos)
		nal := legacyNAL{off: pos + 4, size: n}
		if err := s.captureParamSet(f, typ, nal); err != nil {
			return nil, err
		}
		g.add(nal, typ)
		if _, err := br.Discard(int(4 + n)); err != nil {
			return nil, fmt.Errorf("mp4 legacy recovery: read NAL at %d: %w", pos, err)
		}
		pos += 4 + n
	}
	g.finish()
	if len(s.holes) > 0 && len(s.aus) > 0 {
		s.warnings = append(s.warnings,
			fmt.Sprintf("payload contained %d non-AVCC gaps (audio/resync/noise); "+
				"video recovery tolerated them", len(s.holes)))
	}
	return s, nil
}

func (s *legacyScan) captureParamSet(f *os.File, typ int, nal legacyNAL) error {
	want := false
	var dst *[]byte
	switch s.codec {
	case legacyCodecH264:
		switch typ {
		case 7:
			want, dst = s.sps == nil, &s.sps
		case 8:
			want, dst = s.pps == nil, &s.pps
		}
	case legacyCodecH265:
		switch typ {
		case 32:
			want, dst = s.vps == nil, &s.vps
		case 33:
			want, dst = s.sps == nil, &s.sps
		case 34:
			want, dst = s.pps == nil, &s.pps
		}
	}
	if !want {
		return nil
	}
	if nal.size > legacyMaxParamSetSize {
		s.warnings = append(s.warnings, fmt.Sprintf("ignored oversized parameter set (%d B)", nal.size))
		return nil
	}
	buf := make([]byte, nal.size)
	if _, err := f.ReadAt(buf, nal.off); err != nil {
		return fmt.Errorf("mp4 legacy recovery: read parameter set: %w", err)
	}
	*dst = buf
	return nil
}

// legacyGrouper 按 AU 规则把 NAL 分组成帧。
//
// 关键规则（README §4.3）：一个 VideoFrame 是一次 WriteSample，可能包含多个
// slice。只能按 slice header 的 first_mb_in_slice（H.264）/ first_slice_segment_in_pic_flag
// （H.265）判断 AU 边界，才能避免把多 slice 帧拆成多帧。
type legacyGrouper struct {
	scan           *legacyScan
	f              *os.File
	cur            legacyAU
	hasVCL         bool
	fallbackWarned bool
}

func (g *legacyGrouper) add(nal legacyNAL, typ int) {
	boundary := false
	isVCL := false
	keyFrame := false

	switch g.scan.codec {
	case legacyCodecH264:
		switch typ {
		case 1, 2, 5:
			isVCL = true
			keyFrame = typ == 5
			firstMB, ok := legacyH264FirstMB(g.f, nal)
			if !ok {
				if !g.fallbackWarned {
					g.scan.warnings = append(g.scan.warnings,
						"slice header could not be parsed; fell back to one-VCL-per-AU "+
							"(multi-slice streams would be mis-split)")
					g.fallbackWarned = true
				}
				firstMB = 0
			}
			if firstMB == 0 && g.hasVCL {
				boundary = true
			}
		case 6, 7, 8, 9:
			// 参数集/SEI/AUD 出现在 VCL 之后时属于下一帧。
			if g.hasVCL {
				boundary = true
			}
		}
	case legacyCodecH265:
		switch {
		case typ <= 31:
			isVCL = true
			keyFrame = typ >= 16 && typ <= 23
			first, ok := legacyH265FirstSlice(g.f, nal)
			if !ok {
				if !g.fallbackWarned {
					g.scan.warnings = append(g.scan.warnings,
						"HEVC slice header could not be parsed; fell back to one-VCL-per-AU")
					g.fallbackWarned = true
				}
				first = true
			}
			if first && g.hasVCL {
				boundary = true
			}
		case typ == 32, typ == 33, typ == 34, typ == 35, typ == 39:
			if g.hasVCL {
				boundary = true
			}
		}
	}

	if boundary {
		g.flush()
	}
	// keyFrame 与 hasVCL 必须在本 NAL 成为新 AU 的首个成员之后再设置，
	// 否则 flush 会把这帧的关键帧标记/状态一起清掉。
	if isVCL {
		if keyFrame {
			g.cur.keyFrame = true
		}
		g.hasVCL = true
	}
	g.cur.nals = append(g.cur.nals, nal)
	g.cur.size += 4 + nal.size
}

func (g *legacyGrouper) flush() {
	if g.hasVCL && len(g.cur.nals) > 0 {
		g.scan.aus = append(g.scan.aus, g.cur)
	}
	g.cur = legacyAU{}
	g.hasVCL = false
}

func (g *legacyGrouper) finish() {
	g.flush()
}

func legacyH264FirstMB(f *os.File, nal legacyNAL) (uint64, bool) {
	if nal.size < 2 {
		return 0, false
	}
	n := int64(32)
	if nal.size-1 < n {
		n = nal.size - 1
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, nal.off+1); err != nil {
		return 0, false
	}
	r := newLegacyBitReader(legacyRemoveEmulation(buf))
	ue, ok := r.ue()
	if !ok {
		return 0, false
	}
	return ue, true
}

func legacyH265FirstSlice(f *os.File, nal legacyNAL) (bool, bool) {
	if nal.size < 3 {
		return false, false
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], nal.off+2); err != nil {
		return false, false
	}
	return b[0]&0x80 != 0, true
}

// ---------------------------------------------------------------------------
// 参数集 / stsd / fps
// ---------------------------------------------------------------------------

func legacyBuildVideoSTSD(s *legacyScan) (stsd []byte, width, height int, parserFPS int, err error) {
	var entry box.IBox
	switch s.codec {
	case legacyCodecH264:
		if len(s.sps) == 0 || len(s.pps) == 0 {
			return nil, 0, 0, 0, errors.New("mp4 legacy recovery: H.264 payload has no SPS/PPS")
		}
		cd, perr := h264parser.NewCodecDataFromSPSAndPPS(s.sps, s.pps)
		if perr != nil {
			return nil, 0, 0, 0, fmt.Errorf("mp4 legacy recovery: parse SPS: %w", perr)
		}
		width, height, parserFPS = cd.Width(), cd.Height(), cd.FPS()
		entry = box.CreateVisualSampleEntry(box.TypeAVC1, uint16(width), uint16(height),
			box.CreateDataBox(box.TypeAVCC, cd.Record))
	case legacyCodecH265:
		if len(s.vps) == 0 || len(s.sps) == 0 || len(s.pps) == 0 {
			return nil, 0, 0, 0, errors.New("mp4 legacy recovery: H.265 payload is missing VPS/SPS/PPS")
		}
		cd, perr := h265parser.NewCodecDataFromVPSAndSPSAndPPS(s.vps, s.sps, s.pps)
		if perr != nil {
			return nil, 0, 0, 0, fmt.Errorf("mp4 legacy recovery: parse HEVC parameter sets: %w", perr)
		}
		width, height, parserFPS = cd.Width(), cd.Height(), cd.FPS()
		entry = box.CreateVisualSampleEntry(box.TypeHVC1, uint16(width), uint16(height),
			box.CreateDataBox(box.TypeHVCC, cd.Record))
	default:
		return nil, 0, 0, 0, errors.New("mp4 legacy recovery: unsupported codec")
	}
	if width <= 0 || height <= 0 || width > 0xFFFF || height > 0xFFFF {
		return nil, 0, 0, 0, fmt.Errorf("mp4 legacy recovery: invalid dimensions %dx%d", width, height)
	}
	var buf bytes.Buffer
	if _, err = box.WriteTo(&buf, box.CreateSTSDBox(entry)); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("mp4 legacy recovery: serialize stsd: %w", err)
	}
	return buf.Bytes(), width, height, parserFPS, nil
}

func legacyBuildAudioSTSD(codec string) ([]byte, error) {
	var typ box.BoxType
	switch codec {
	case "pcm_alaw":
		typ = box.TypeALAW
	case "pcm_ulaw":
		typ = box.TypeULAW
	default:
		return nil, fmt.Errorf("unsupported audio codec %q", codec)
	}
	entry := box.CreateAudioSampleEntry(typ, 1, 16, legacyAudioRate, nil)
	var buf bytes.Buffer
	if _, err := box.WriteTo(&buf, box.CreateSTSDBox(entry)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func legacyFourCC(codec legacyCodec) string {
	if codec == legacyCodecH265 {
		return "hvc1"
	}
	return "avc1"
}

func legacyAudioFourCC(codec string) string {
	if codec == "pcm_ulaw" {
		return "ulaw"
	}
	return "alaw"
}

// legacyResolveFPS 依次使用调用方指定值、SPS VUI 精确帧率、解析器整数帧率、
// 默认 25 fps。fractional fps 必须来自 time_scale/(2*num_units_in_tick)，
// 不能直接用整数 FPS（29.97 会被截成 29）。
func legacyResolveFPS(s *legacyScan, requested float64, parserFPS int) (float64, string) {
	if requested > 0 {
		if requested < 1 || requested > 240 {
			return legacyDefaultFPS, fmt.Sprintf("requested fps %g is out of range; using %.0f", requested, legacyDefaultFPS)
		}
		return requested, ""
	}
	if s.codec == legacyCodecH264 {
		if fps, ok := legacyH264VUIFPS(s.sps); ok {
			return fps, fmt.Sprintf("video timestamps estimated from SPS VUI frame rate %.6g fps", fps)
		}
	}
	if parserFPS > 0 {
		return float64(parserFPS), fmt.Sprintf("video timestamps estimated from SPS integer frame rate %d fps", parserFPS)
	}
	return legacyDefaultFPS, fmt.Sprintf("no frame rate in SPS; assuming %.0f fps", legacyDefaultFPS)
}

// ---------------------------------------------------------------------------
// 音频启发式恢复
// ---------------------------------------------------------------------------

// legacyRecoverAudio 从非 AVCC 空洞里尝试恢复定长 G.711 音频。
//
// 判据（README §4.6）：所有候选空洞都是同一帧长 F 的整数倍，且字节以
// A-law（0x55/0xD5）或 µ-law（0xFF/0x7F）静音字节为主。任一条件不满足
// 就放弃音频并告警——绝不静默产出一条错误的音轨。
func legacyRecoverAudio(f *os.File, holes []legacyHole, warnings []string) ([]legacyAudioFrame, string, []string) {
	if len(holes) == 0 {
		return nil, "", append(warnings, "audio not recovered: payload contains no non-AVCC frames")
	}
	var candidates []legacyHole
	ignored := 0
	for _, h := range holes {
		if h.size > legacyMaxAudioHole {
			ignored++
			continue
		}
		candidates = append(candidates, h)
	}
	if ignored > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"audio recovery ignored %d oversized gap(s) (>%d B), likely truncation artifacts",
			ignored, legacyMaxAudioHole))
	}
	if len(candidates) == 0 {
		return nil, "", append(warnings, "audio not recovered: no plausible audio-sized gaps")
	}

	g := int64(0)
	for _, h := range candidates {
		g = legacyGCD(g, h.size)
	}
	if g < legacyAudioMinFrame || g > legacyAudioMaxFrame {
		return nil, "", append(warnings, fmt.Sprintf(
			"audio not recovered: gap sizes do not form a fixed G.711 frame (gcd=%d)", g))
	}

	// 内容启发式：抽样每个空洞开头最多 4 KiB，统计静音字节占比。
	var alaw, ulaw, total int64
	for _, h := range candidates {
		n := h.size
		if n > 4096 {
			n = 4096
		}
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, h.off); err != nil {
			return nil, "", append(warnings, "audio not recovered: read candidate gap: "+err.Error())
		}
		for _, b := range buf {
			switch b {
			case 0x55, 0xD5:
				alaw++
			case 0xFF, 0x7F:
				ulaw++
			}
			total++
		}
	}
	codec := ""
	switch {
	case total > 0 && alaw*10 >= total*9:
		codec = "pcm_alaw"
	case total > 0 && ulaw*10 >= total*9:
		codec = "pcm_ulaw"
	default:
		return nil, "", append(warnings,
			"audio not recovered: gap bytes are not G.711 A-law/µ-law silence (AAC/other codecs cannot be framed without a sample table)")
	}

	var frames []legacyAudioFrame
	for _, h := range candidates {
		for off := int64(0); off+g <= h.size; off += g {
			frames = append(frames, legacyAudioFrame{off: h.off + off, size: g})
		}
	}
	if len(frames) < 2 {
		return nil, "", append(warnings, "audio not recovered: too few complete G.711 frames")
	}
	return frames, codec, warnings
}

// ---------------------------------------------------------------------------
// 样本表 / 写盘
// ---------------------------------------------------------------------------

type legacyPart struct {
	spans []legacySpan
	size  int64
}

func legacyBuildRecords(s *legacyScan, fps float64, audioFrames []legacyAudioFrame) ([]mp4pkg.IndexRecord, []legacyPart) {
	records := make([]mp4pkg.IndexRecord, 0, len(s.aus)+len(audioFrames))
	parts := make([]legacyPart, 0, len(s.aus)+len(audioFrames))
	var compact uint64

	for i, au := range s.aus {
		dts := uint32(math.Round(float64(i) * 1000.0 / fps))
		flags := uint8(0)
		if au.keyFrame {
			flags = mp4pkg.IndexFlagKeyFrame
		}
		part := legacyPart{size: au.size}
		for _, nal := range au.nals {
			part.spans = append(part.spans, legacySpan{off: nal.off - 4, size: 4 + nal.size})
		}
		records = append(records, mp4pkg.IndexRecord{
			Flags:   flags,
			TrackID: 1,
			Size:    uint32(au.size),
			Offset:  compact,
			DTS:     dts,
		})
		parts = append(parts, part)
		compact += uint64(au.size)
	}

	if len(audioFrames) > 0 {
		frameMs := uint32(math.Round(float64(audioFrames[0].size) * 1000.0 / legacyAudioRate))
		if frameMs < 1 {
			frameMs = 1
		}
		for i, af := range audioFrames {
			records = append(records, mp4pkg.IndexRecord{
				TrackID: 2,
				Size:    uint32(af.size),
				Offset:  compact,
				DTS:     uint32(i) * frameMs,
			})
			parts = append(parts, legacyPart{spans: []legacySpan{{off: af.off, size: af.size}}, size: af.size})
			compact += uint64(af.size)
		}
	}
	return records, parts
}

// legacyWriteOutput 以原子方式写 ftyp|moov|mdat 头 + 逐样本载荷。
func legacyWriteOutput(src *os.File, outputPath string, moov []byte, parts []legacyPart) (err error) {
	dir := filepath.Dir(outputPath)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(outputPath)+".recover-*")
	if err != nil {
		return fmt.Errorf("mp4 legacy recovery: create output: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	w := bufio.NewWriterSize(tmp, 512<<10)
	if _, err = w.Write(moov); err != nil {
		return fmt.Errorf("mp4 legacy recovery: write moov: %w", err)
	}
	for _, part := range parts {
		for _, span := range part.spans {
			if _, err = io.CopyN(w, io.NewSectionReader(src, span.off, span.size), span.size); err != nil {
				return fmt.Errorf("mp4 legacy recovery: copy sample at %d: %w", span.off, err)
			}
		}
	}
	if err = w.Flush(); err != nil {
		return fmt.Errorf("mp4 legacy recovery: flush output: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("mp4 legacy recovery: sync output: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("mp4 legacy recovery: close output: %w", err)
	}
	if err = os.Rename(tmpName, outputPath); err != nil {
		return fmt.Errorf("mp4 legacy recovery: rename output: %w", err)
	}
	committed = true
	return nil
}

func legacySamePath(a, b string) (bool, error) {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	if aa == bb {
		return true, nil
	}
	as, err := os.Stat(aa)
	if err != nil {
		return false, nil // 输出文件尚不存在，不可能与源相同
	}
	bs, err := os.Stat(bb)
	if err != nil {
		return false, nil
	}
	return os.SameFile(as, bs), nil
}

func legacyGCD(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// ---------------------------------------------------------------------------
// H.264 SPS VUI 帧率解析
// ---------------------------------------------------------------------------

func legacyH264VUIFPS(sps []byte) (float64, bool) {
	if len(sps) < 5 {
		return 0, false
	}
	r := newLegacyBitReader(legacyRemoveEmulation(sps[1:]))
	r.skip(8)                 // profile_idc
	r.skip(8)                 // constraint flags
	r.skip(8)                 // level_idc
	if _, ok := r.ue(); !ok { // seq_parameter_set_id
		return 0, false
	}

	var chromaFormat uint64 = 1
	switch sps[1] { // profile_idc
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		if chromaFormat, r.ok = r.ue(); !r.ok {
			return 0, false
		}
		if chromaFormat == 3 && !r.skip(1) {
			return 0, false
		}
		if _, ok := r.ue(); !ok { // bit_depth_luma_minus8
			return 0, false
		}
		if _, ok := r.ue(); !ok { // bit_depth_chroma_minus8
			return 0, false
		}
		if !r.skip(1) { // qpprime_y_zero_transform_bypass_flag
			return 0, false
		}
		present, ok := r.bit()
		if !ok {
			return 0, false
		}
		if present == 1 {
			n := 8
			if chromaFormat == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				flag, ok := r.bit()
				if !ok {
					return 0, false
				}
				if flag == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					if !r.skipScalingList(size) {
						return 0, false
					}
				}
			}
		}
	}

	if _, ok := r.ue(); !ok { // log2_max_frame_num_minus4
		return 0, false
	}
	pocType, ok := r.ue()
	if !ok {
		return 0, false
	}
	switch pocType {
	case 0:
		if _, ok := r.ue(); !ok { // log2_max_pic_order_cnt_lsb_minus4
			return 0, false
		}
	case 1:
		if !r.skip(1) { // delta_pic_order_always_zero_flag
			return 0, false
		}
		if _, ok := r.se(); !ok {
			return 0, false
		}
		if _, ok := r.se(); !ok {
			return 0, false
		}
		n, ok := r.ue()
		if !ok {
			return 0, false
		}
		for i := uint64(0); i < n; i++ {
			if _, ok := r.se(); !ok {
				return 0, false
			}
		}
	}

	if _, ok := r.ue(); !ok { // max_num_ref_frames
		return 0, false
	}
	if !r.skip(1) { // gaps_in_frame_num_value_allowed_flag
		return 0, false
	}
	if _, ok := r.ue(); !ok { // pic_width_in_mbs_minus1
		return 0, false
	}
	if _, ok := r.ue(); !ok { // pic_height_in_map_units_minus1
		return 0, false
	}
	frameMbsOnly, ok := r.bit()
	if !ok {
		return 0, false
	}
	if frameMbsOnly == 0 && !r.skip(1) { // mb_adaptive_frame_field_flag
		return 0, false
	}
	if !r.skip(1) { // direct_8x8_inference_flag
		return 0, false
	}
	cropping, ok := r.bit()
	if !ok {
		return 0, false
	}
	if cropping == 1 {
		for i := 0; i < 4; i++ {
			if _, ok := r.ue(); !ok {
				return 0, false
			}
		}
	}
	vui, ok := r.bit()
	if !ok || vui == 0 {
		return 0, false
	}

	if flag, ok := r.bit(); !ok {
		return 0, false
	} else if flag == 1 { // aspect_ratio_info_present_flag
		idc, ok := r.bits(8)
		if !ok {
			return 0, false
		}
		if idc == 255 && !r.skip(32) { // sar_width + sar_height
			return 0, false
		}
	}
	if flag, ok := r.bit(); !ok { // overscan_info_present_flag
		return 0, false
	} else if flag == 1 && !r.skip(1) { // overscan_appropriate_flag
		return 0, false
	}
	if flag, ok := r.bit(); !ok { // video_signal_type_present_flag
		return 0, false
	} else if flag == 1 {
		if !r.skip(3) { // video_format
			return 0, false
		}
		if !r.skip(1) { // video_full_range_flag
			return 0, false
		}
		desc, ok := r.bit()
		if !ok {
			return 0, false
		}
		if desc == 1 && !r.skip(24) { // colour_primaries + transfer + matrix
			return 0, false
		}
	}
	if flag, ok := r.bit(); !ok { // chroma_loc_info_present_flag
		return 0, false
	} else if flag == 1 {
		if _, ok := r.ue(); !ok {
			return 0, false
		}
		if _, ok := r.ue(); !ok {
			return 0, false
		}
	}
	timing, ok := r.bit()
	if !ok || timing == 0 {
		return 0, false
	}
	numUnits, ok := r.bits(32)
	if !ok {
		return 0, false
	}
	timeScale, ok := r.bits(32)
	if !ok || numUnits == 0 {
		return 0, false
	}
	return float64(timeScale) / float64(numUnits) / 2.0, true
}

func legacyRemoveEmulation(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if i+2 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 3 {
			out = append(out, 0, 0)
			i += 2
			continue
		}
		out = append(out, b[i])
	}
	return out
}

type legacyBitReader struct {
	b   []byte
	pos int
	ok  bool
}

func newLegacyBitReader(b []byte) legacyBitReader {
	return legacyBitReader{b: b, ok: true}
}

func (r *legacyBitReader) bit() (uint64, bool) {
	if !r.ok {
		return 0, false
	}
	if r.pos >= len(r.b)*8 {
		r.ok = false
		return 0, false
	}
	v := uint64((r.b[r.pos/8] >> (7 - uint(r.pos%8))) & 1)
	r.pos++
	return v, true
}

func (r *legacyBitReader) bits(n int) (uint64, bool) {
	var v uint64
	for i := 0; i < n; i++ {
		b, ok := r.bit()
		if !ok {
			return 0, false
		}
		v = v<<1 | b
	}
	return v, true
}

func (r *legacyBitReader) skip(n int) bool {
	if !r.ok {
		return false
	}
	if r.pos+n > len(r.b)*8 {
		r.ok = false
		return false
	}
	r.pos += n
	return true
}

// ue 读取 Exp-Golomb 无符号数。
func (r *legacyBitReader) ue() (uint64, bool) {
	var zeros int
	for {
		b, ok := r.bit()
		if !ok {
			return 0, false
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 63 {
			r.ok = false
			return 0, false
		}
	}
	if zeros == 0 {
		return 0, true
	}
	v, ok := r.bits(zeros)
	if !ok {
		return 0, false
	}
	return (1 << uint(zeros)) - 1 + v, true
}

func (r *legacyBitReader) se() (int64, bool) {
	v, ok := r.ue()
	if !ok {
		return 0, false
	}
	if v&1 == 1 {
		return int64(v+1) / 2, true
	}
	return -int64(v / 2), true
}

func (r *legacyBitReader) skipScalingList(size int) bool {
	lastScale, nextScale := int64(8), int64(8)
	for i := 0; i < size; i++ {
		if nextScale != 0 {
			delta, ok := r.se()
			if !ok {
				return false
			}
			nextScale = (lastScale + delta + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
	return true
}
