package mp4

import (
	"encoding/binary"
	"io"
	"os"

	"m7s.live/v5/pkg"
	. "m7s.live/v5/plugin/mp4/pkg/box"
)

const (
	FLAG_FRAGMENT Flag = (1 << 1)
	FLAG_KEYFRAME Flag = (1 << 3)
	FLAG_CUSTOM   Flag = (1 << 5)
	FLAG_DASH     Flag = (1 << 11)
)

type (
	Flag uint32

	Muxer struct {
		nextTrackId    uint32
		nextFragmentId uint32
		CurrentOffset  int64
		Tracks         map[uint32]*Track
		Flag
		fragDuration uint32
		moov         *BasicBox
		mdatOffset   uint64
		mdatSize     uint64
	}
)

func (m Muxer) isFragment() bool {
	return (m.Flag & FLAG_FRAGMENT) != 0
}

func (m Muxer) isDash() bool {
	return (m.Flag & FLAG_DASH) != 0
}

func (m Muxer) has(flag Flag) bool {
	return (m.Flag & flag) != 0
}

func NewMuxer(flag Flag) *Muxer {
	return &Muxer{
		nextTrackId:    1,
		nextFragmentId: 1,
		Tracks:         make(map[uint32]*Track),
		Flag:           flag,
		fragDuration:   2000,
	}
}

func (m *Muxer) WriteInitSegment(w io.Writer) (err error) {
	var n int
	var ftypBox []byte
	if m.isFragment() {
		// 对于 FMP4,使用 iso5 作为主品牌,兼容 iso5, iso6, mp41
		ftypBox = MakeFtypBox(TypeISO5, 0x200, TypeISO5, TypeISO6, TypeMP41)
	} else {
		// 对于普通 MP4,使用 isom 作为主品牌
		ftypBox = MakeFtypBox(TypeISOM, 0x200, TypeISOM, TypeISO2, TypeAVC1, TypeMP41)
	}
	n, err = w.Write(ftypBox)
	if err != nil {
		return
	}
	m.CurrentOffset = int64(n)
	if !m.isFragment() {
		n, err = w.Write((new(FreeBox)).Encode())
		if err != nil {
			return
		}
		m.CurrentOffset += int64(n)
		err = m.WriteEmptyMdat(w)
		if err != nil {
			return
		}
	}
	return
}

func (m *Muxer) WriteEmptyMdat(w io.Writer) (err error) {
	// Write mdat box header with initial size
	mdat := MediaDataBox(0)
	mdatlen, mdatBox := mdat.Encode()
	m.mdatOffset = uint64(m.CurrentOffset + 8)
	m.mdatSize = 0
	var n int
	n, err = w.Write(mdatBox[0:mdatlen])
	if err != nil {
		return
	}
	m.CurrentOffset += int64(n)
	return
}

func (m *Muxer) AddTrack(cid MP4_CODEC_TYPE) *Track {
	track := &Track{
		Cid:       cid,
		TrackId:   m.nextTrackId,
		Timescale: 1000,
	}
	if m.isFragment() || m.isDash() {
		track.writer = NewFmp4WriterSeeker(1024 * 1024)
	}
	m.Tracks[m.nextTrackId] = track
	m.nextTrackId++
	return track
}

func (m *Muxer) WriteSample(w io.Writer, t *Track, sample Sample) (err error) {
	if m.isFragment() {
		// For fragmented MP4, write to track's buffer
		if sample.Offset, err = t.writer.Seek(0, io.SeekCurrent); err != nil {
			return
		}
		if sample.Size, err = t.writer.Write(sample.Data); err != nil {
			return
		}
		defer func() {
			// For fragmented MP4, check if we should create a new fragment
			if sample.KeyFrame && t.Duration >= m.fragDuration {
				err = m.flushFragment(w)
			}
		}()
	} else {
		// For regular MP4, write directly to output
		sample.Offset = m.CurrentOffset
		sample.Size, err = w.Write(sample.Data)
		if err != nil {
			return
		}
		m.CurrentOffset += int64(sample.Size)
	}
	sample.Data = nil
	t.AddSampleEntry(sample)
	return
}

func (m *Muxer) reWriteMdatSize(w io.WriteSeeker) (err error) {
	m.mdatSize = uint64(m.CurrentOffset) - (m.mdatOffset)
	if m.mdatSize+BasicBoxLen > 0xFFFFFFFF {
		_, mdatBox := MediaDataBox(m.mdatSize).Encode()
		if _, err = w.Seek(int64(m.mdatOffset-16), io.SeekStart); err != nil {
			return
		}
		if _, err = w.Write(mdatBox); err != nil {
			return
		}
		if _, err = w.Seek(m.CurrentOffset, io.SeekStart); err != nil {
			return
		}
	} else {
		if _, err = w.Seek(int64(m.mdatOffset-8), io.SeekStart); err != nil {
			return
		}
		tmpdata := make([]byte, 4)
		binary.BigEndian.PutUint32(tmpdata, uint32(m.mdatSize)+BasicBoxLen)
		if _, err = w.Write(tmpdata); err != nil {
			return
		}
		if _, err = w.Seek(m.CurrentOffset, io.SeekStart); err != nil {
			return
		}
	}
	return
}

func (m *Muxer) ReWriteWithMoov(f io.WriteSeeker, r io.Reader) (err error) {
	if m.isFragment() {
		return pkg.ErrSkip
	}
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return
	}
	_, err = io.CopyN(f, r, int64(m.mdatOffset)-16)
	if err != nil {
		return
	}
	for _, track := range m.Tracks {
		for i := range len(track.Samplelist) {
			track.Samplelist[i].Offset += int64(m.moov.Size)
		}
	}
	err = m.WriteMoov(f)
	if err != nil {
		return
	}
	_, err = io.CopyN(f, r, int64(m.mdatSize)+16)
	return
}

func (m *Muxer) makeMvex() []byte {
	mvex := BasicBox{Type: TypeMVEX}
	trexs := make([]byte, 0, 64)
	for i := uint32(1); i < m.nextTrackId; i++ {
		if track := m.Tracks[i]; track != nil {
			trex := NewTrackExtendsBox(track.TrackId)
			trex.DefaultSampleDescriptionIndex = 1
			trex.DefaultSampleDuration = 0
			trex.DefaultSampleSize = 0
			if track.Cid.IsVideo() {
				trex.DefaultSampleFlags = 0x00010000 // NonSyncSampleFlags in mp4ff
			} else {
				trex.DefaultSampleFlags = 0x02000000 // SyncSampleFlags in mp4ff
			}
			_, boxData := trex.Encode()
			trexs = append(trexs, boxData...)
		}
	}
	mvex.Size = 8 + uint64(len(trexs))
	offset, mvexBox := mvex.Encode()
	copy(mvexBox[offset:], trexs)
	return mvexBox
}

func (m *Muxer) makeTrak(track *Track) []byte {
	edts := []byte{}
	if m.isDash() || m.isFragment() {
		// track.makeEmptyStblTable()
	} else {
		if len(track.Samplelist) > 0 {
			track.makeStblTable()
			edts = track.makeEdtsBox()
		}
	}

	tkhd := track.makeTkhdBox()
	mdia := track.makeMdiaBox()

	trak := BasicBox{Type: TypeTRAK}
	trak.Size = 8 + uint64(len(tkhd)+len(edts)+len(mdia))
	offset, trakBox := trak.Encode()
	copy(trakBox[offset:], tkhd)
	offset += len(tkhd)
	copy(trakBox[offset:], edts)
	offset += len(edts)
	copy(trakBox[offset:], mdia)
	return trakBox
}

func (m *Muxer) GetMoovSize() int {
	moovsize := FullBoxLen + 96
	if m.isDash() || m.isFragment() {
		moovsize += 64
	}
	traks := make([][]byte, len(m.Tracks))
	for i := uint32(1); i < m.nextTrackId; i++ {
		traks[i-1] = m.makeTrak(m.Tracks[i])
		moovsize += len(traks[i-1])
	}
	return int(8 + uint64(moovsize))
}

func (m *Muxer) WriteMoov(w io.Writer) (err error) {
	var mvhd []byte
	var mvex []byte
	if m.isDash() || m.isFragment() {
		mvhd = MakeMvhdBox(m.nextTrackId, 0)
		mvex = m.makeMvex()
	} else {
		maxdurtaion := uint32(0)
		for _, track := range m.Tracks {
			if maxdurtaion < track.Duration {
				maxdurtaion = track.Duration
			}
		}
		mvhd = MakeMvhdBox(m.nextTrackId, maxdurtaion)
	}
	moovsize := len(mvhd) + len(mvex)
	traks := make([][]byte, len(m.Tracks))
	for i := uint32(1); i < m.nextTrackId; i++ {
		traks[i-1] = m.makeTrak(m.Tracks[i])
		moovsize += len(traks[i-1])
	}

	moov := BasicBox{Type: TypeMOOV}
	moov.Size = 8 + uint64(moovsize)
	offset, moovBox := moov.Encode()
	copy(moovBox[offset:], mvhd)
	offset += len(mvhd)
	for _, trak := range traks {
		copy(moovBox[offset:], trak)
		offset += len(trak)
	}
	if mvex != nil {
		copy(moovBox[offset:], mvex)
	}

	// Write moov box
	_, err = w.Write(moovBox)
	m.moov = &moov
	m.CurrentOffset += int64(moov.Size)
	return
}

func (m *Muxer) WriteTrailer(file *os.File) (err error) {
	if m.isFragment() {
		// Flush any remaining samples
		if err = m.flushFragment(file); err != nil {
			return err
		}

		// Write mfra box
		mfraSize := 0
		tfras := make([][]byte, len(m.Tracks))
		for i := uint32(1); i < m.nextTrackId; i++ {
			if track := m.Tracks[i]; track != nil && len(track.fragments) > 0 {
				tfras[i-1] = track.makeTfraBox()
				mfraSize += len(tfras[i-1])
			}
		}

		// Only write mfra if we have fragments
		if mfraSize > 0 {
			mfro := MakeMfroBox(uint32(mfraSize) + 16)
			mfraSize += len(mfro)
			mfra := BasicBox{Type: TypeMFRA}
			mfra.Size = 8 + uint64(mfraSize)
			offset, mfraBox := mfra.Encode()
			for _, tfra := range tfras {
				if tfra == nil {
					continue
				}
				copy(mfraBox[offset:], tfra)
				offset += len(tfra)
			}
			copy(mfraBox[offset:], mfro)
			if _, err = file.Write(mfraBox); err != nil {
				return err
			}
		}

		// Clean up any remaining buffers
		for i := uint32(1); i < m.nextTrackId; i++ {
			if track := m.Tracks[i]; track != nil && track.writer != nil {
				if ws, ok := track.writer.(*Fmp4WriterSeeker); ok {
					ws.Buffer = nil
				}
			}
		}
	} else {
		if err = m.reWriteMdatSize(file); err != nil {
			return err
		}
		return m.WriteMoov(file)
	}
	return nil
}

func (m *Muxer) flushFragment(w io.Writer) (err error) {
	// Check if there are any samples to write
	hasSamples := false
	for i := uint32(1); i < m.nextTrackId; i++ {
		if len(m.Tracks[i].Samplelist) > 0 {
			hasSamples = true
			break
		}
	}
	if !hasSamples {
		return nil
	}

	// Write moov box if not written yet
	if m.moov == nil {
		if err = m.WriteMoov(w); err != nil {
			return err
		}
	}
	// Calculate mdat size first
	var mdatSize uint64 = 8 // mdat box header
	for i := uint32(1); i < m.nextTrackId; i++ {
		if len(m.Tracks[i].Samplelist) == 0 {
			continue
		}
		ws := m.Tracks[i].writer.(*Fmp4WriterSeeker)
		mdatSize += uint64(len(ws.Buffer))
	}

	// Write moof box
	mfhd := MakeMfhdBox(m.nextFragmentId)
	trafs := make([][]byte, len(m.Tracks))
	moofSize := len(mfhd)
	trunOffsets := make([]int, len(m.Tracks)) // track index -> trun data_offset position in moof box
	var boxOffset int = 8 + len(mfhd)         // 8 for moof header
	for i := uint32(1); i < m.nextTrackId; i++ {
		if len(m.Tracks[i].Samplelist) == 0 {
			continue
		}
		track := m.Tracks[i]
		// 传递 moof 偏移和 mdat 大小
		traf := track.makeTraf(&trunOffsets[int(i-1)]) // +8 for moof box header
		// Record trun data_offset position: current offset + 16 (after trun header)
		trafs[i-1] = traf
		trunOffsets[int(i-1)] += boxOffset
		boxOffset += len(traf)
		moofSize += len(traf)
	}

	// Write moof box
	moof := BasicBox{Type: TypeMOOF}
	moof.Size = uint64(moofSize + 8) // Add 8 for moof box header
	offset, moofBox := moof.Encode()
	copy(moofBox[offset:], mfhd)
	offset += len(mfhd)
	for i, traf := range trafs {
		if traf == nil {
			continue
		}
		copy(moofBox[offset:], traf)
		// Update trun data_offset
		binary.BigEndian.PutUint32(moofBox[trunOffsets[i]:trunOffsets[i]+4], uint32(moof.Size)+8) // +8 for mdat header
		offset += len(traf)
	}

	if _, err = w.Write(moofBox); err != nil {
		return err
	}

	// Write mdat box
	mdat := BasicBox{Type: TypeMDAT}
	mdat.Size = mdatSize
	offset, mdatBox := mdat.Encode()
	if _, err = w.Write(mdatBox[:offset]); err != nil {
		return err
	}

	// Write sample data
	var sampleOffset int64 = 0
	for i := uint32(1); i < m.nextTrackId; i++ {
		if len(m.Tracks[i].Samplelist) == 0 {
			continue
		}
		track := m.Tracks[i]
		ws := track.writer.(*Fmp4WriterSeeker)

		// Update sample offsets relative to mdat start
		for j := range track.Samplelist {
			track.Samplelist[j].Offset = sampleOffset
			sampleOffset += int64(track.Samplelist[j].Size)
		}

		if _, err = w.Write(ws.Buffer); err != nil {
			return err
		}

		// Record fragment info
		if len(track.Samplelist) > 0 {
			firstPts := track.Samplelist[0].PTS
			firstDts := track.Samplelist[0].DTS
			lastPts := track.Samplelist[len(track.Samplelist)-1].PTS
			lastDts := track.Samplelist[len(track.Samplelist)-1].DTS
			frag := Fragment{
				Offset:   uint64(m.CurrentOffset),
				Duration: track.Duration,
				FirstDts: firstDts,
				FirstPts: firstPts,
				LastPts:  lastPts,
				LastDts:  lastDts,
			}
			track.fragments = append(track.fragments, frag)
		}

		// Clear track buffers
		ws.Buffer = ws.Buffer[:0]
		ws.Offset = 0
		track.Samplelist = track.Samplelist[:0]
		track.Duration = 0
	}
	m.CurrentOffset += int64(moof.Size) + int64(mdatSize)
	m.nextFragmentId++
	return nil
}

// SetFragmentDuration sets the target duration for each fragment in milliseconds
func (m *Muxer) SetFragmentDuration(duration uint32) {
	m.fragDuration = duration
}
