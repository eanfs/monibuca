package box

import (
	"encoding/binary"
	"io"
	"time"
)

// Metadata holds various metadata information for MP4
type Metadata struct {
	Title     string            // 标题
	Artist    string            // 艺术家/作者
	Album     string            // 专辑
	Date      string            // 日期
	Comment   string            // 注释/描述
	Genre     string            // 类型
	Copyright string            // 版权信息
	Encoder   string            // 编码器
	Writer    string            // 作词者
	Producer  string            // 制作人
	Performer string            // 表演者
	Grouping  string            // 分组
	Lyrics    string            // 歌词
	Keywords  string            // 关键词
	Location  string            // 位置信息
	Rating    uint8             // 评级 (0-5)
	Custom    map[string]string // 自定义键值对
}

// Text Data Box - for storing text metadata
type TextDataBox struct {
	FullBox
	Text string
}

// Metadata Data Box - for storing binary metadata with type indicator
type MetadataDataBox struct {
	FullBox
	DataType uint32 // Data type indicator
	Country  uint32 // Country code
	Language uint32 // Language code
	Data     []byte // Actual data
}

// Copyright Box
type CopyrightBox struct {
	FullBox
	Language [3]byte
	Notice   string
}

// Custom Metadata Box (iTunes-style ---- box)
type CustomMetadataBox struct {
	BaseBox
	Mean string // Mean (namespace)
	Name string // Name (key)
	Data []byte // Data
}

// Create functions

func CreateTextDataBox(boxType BoxType, text string) *TextDataBox {
	return &TextDataBox{
		FullBox: FullBox{
			BaseBox: BaseBox{
				typ:  boxType,
				size: uint32(FullBoxLen + len(text)),
			},
			Version: 0,
			Flags:   [3]byte{0, 0, 0},
		},
		Text: text,
	}
}

func CreateMetadataDataBox(dataType uint32, data []byte) *MetadataDataBox {
	return &MetadataDataBox{
		FullBox: FullBox{
			BaseBox: BaseBox{
				typ:  f("data"),
				size: uint32(FullBoxLen + 8 + len(data)), // 8 bytes for type+country+language
			},
			Version: 0,
			Flags:   [3]byte{0, 0, 0},
		},
		DataType: dataType,
		Country:  0,
		Language: 0,
		Data:     data,
	}
}

func CreateCopyrightBox(language [3]byte, notice string) *CopyrightBox {
	return &CopyrightBox{
		FullBox: FullBox{
			BaseBox: BaseBox{
				typ:  TypeCPRT,
				size: uint32(FullBoxLen + 3 + 1 + len(notice)), // 3 for language, 1 for null terminator
			},
			Version: 0,
			Flags:   [3]byte{0, 0, 0},
		},
		Language: language,
		Notice:   notice,
	}
}

func CreateCustomMetadataBox(mean, name string, data []byte) *CustomMetadataBox {
	size := uint32(BasicBoxLen + 4 + len(mean) + 4 + len(name) + len(data))
	return &CustomMetadataBox{
		BaseBox: BaseBox{
			typ:  TypeMETA_CUST,
			size: size,
		},
		Mean: mean,
		Name: name,
		Data: data,
	}
}

// WriteTo methods

func (box *TextDataBox) WriteTo(w io.Writer) (n int64, err error) {
	nn, err := w.Write([]byte(box.Text))
	return int64(nn), err
}

func (box *MetadataDataBox) WriteTo(w io.Writer) (n int64, err error) {
	var tmp [8]byte
	binary.BigEndian.PutUint32(tmp[0:4], box.DataType)
	binary.BigEndian.PutUint32(tmp[4:8], box.Country)
	// Language field is implicit zero

	nn, err := w.Write(tmp[:8])
	if err != nil {
		return int64(nn), err
	}
	n = int64(nn)

	nn, err = w.Write(box.Data)
	return n + int64(nn), err
}

func (box *CopyrightBox) WriteTo(w io.Writer) (n int64, err error) {
	// Write language code
	nn, err := w.Write(box.Language[:])
	if err != nil {
		return int64(nn), err
	}
	n = int64(nn)

	// Write notice + null terminator
	nn, err = w.Write([]byte(box.Notice + "\x00"))
	return n + int64(nn), err
}

func (box *CustomMetadataBox) WriteTo(w io.Writer) (n int64, err error) {
	var tmp [4]byte

	// Write mean length + mean
	binary.BigEndian.PutUint32(tmp[:], uint32(len(box.Mean)))
	nn, err := w.Write(tmp[:])
	if err != nil {
		return int64(nn), err
	}
	n = int64(nn)

	nn, err = w.Write([]byte(box.Mean))
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)

	// Write name length + name
	binary.BigEndian.PutUint32(tmp[:], uint32(len(box.Name)))
	nn, err = w.Write(tmp[:])
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)

	nn, err = w.Write([]byte(box.Name))
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)

	// Write data
	nn, err = w.Write(box.Data)
	return n + int64(nn), err
}

// Unmarshal methods

func (box *TextDataBox) Unmarshal(buf []byte) (IBox, error) {
	box.Text = string(buf)
	return box, nil
}

func (box *MetadataDataBox) Unmarshal(buf []byte) (IBox, error) {
	if len(buf) < 8 {
		return nil, io.ErrShortBuffer
	}

	box.DataType = binary.BigEndian.Uint32(buf[0:4])
	box.Country = binary.BigEndian.Uint32(buf[4:8])
	box.Data = buf[8:]
	return box, nil
}

func (box *CopyrightBox) Unmarshal(buf []byte) (IBox, error) {
	if len(buf) < 4 {
		return nil, io.ErrShortBuffer
	}

	copy(box.Language[:], buf[0:3])
	// Find null terminator
	for i := 3; i < len(buf); i++ {
		if buf[i] == 0 {
			box.Notice = string(buf[3:i])
			break
		}
	}
	if box.Notice == "" && len(buf) > 3 {
		box.Notice = string(buf[3:])
	}
	return box, nil
}

func (box *CustomMetadataBox) Unmarshal(buf []byte) (IBox, error) {
	if len(buf) < 8 {
		return nil, io.ErrShortBuffer
	}

	offset := 0

	// Read mean length + mean
	meanLen := binary.BigEndian.Uint32(buf[offset:])
	offset += 4
	if offset+int(meanLen) > len(buf) {
		return nil, io.ErrShortBuffer
	}
	box.Mean = string(buf[offset : offset+int(meanLen)])
	offset += int(meanLen)

	// Read name length + name
	if offset+4 > len(buf) {
		return nil, io.ErrShortBuffer
	}
	nameLen := binary.BigEndian.Uint32(buf[offset:])
	offset += 4
	if offset+int(nameLen) > len(buf) {
		return nil, io.ErrShortBuffer
	}
	box.Name = string(buf[offset : offset+int(nameLen)])
	offset += int(nameLen)

	// Read remaining data
	box.Data = buf[offset:]
	return box, nil
}

// Create metadata entries from Metadata struct
func CreateMetadataEntries(metadata *Metadata) []IBox {
	var entries []IBox

	// Standard text metadata
	if metadata.Title != "" {
		entries = append(entries, CreateTextDataBox(TypeTITL, metadata.Title))
	}
	if metadata.Artist != "" {
		entries = append(entries, CreateTextDataBox(TypeART, metadata.Artist))
	}
	if metadata.Album != "" {
		entries = append(entries, CreateTextDataBox(TypeALB, metadata.Album))
	}
	if metadata.Date != "" {
		entries = append(entries, CreateTextDataBox(TypeDAY, metadata.Date))
	}
	if metadata.Comment != "" {
		entries = append(entries, CreateTextDataBox(TypeCMT, metadata.Comment))
	}
	if metadata.Genre != "" {
		entries = append(entries, CreateTextDataBox(TypeGEN, metadata.Genre))
	}
	if metadata.Encoder != "" {
		entries = append(entries, CreateTextDataBox(TypeENCO, metadata.Encoder))
	}
	if metadata.Writer != "" {
		entries = append(entries, CreateTextDataBox(TypeWRT, metadata.Writer))
	}
	if metadata.Producer != "" {
		entries = append(entries, CreateTextDataBox(TypePRD, metadata.Producer))
	}
	if metadata.Performer != "" {
		entries = append(entries, CreateTextDataBox(TypePRF, metadata.Performer))
	}
	if metadata.Grouping != "" {
		entries = append(entries, CreateTextDataBox(TypeGRP, metadata.Grouping))
	}
	if metadata.Lyrics != "" {
		entries = append(entries, CreateTextDataBox(TypeLYR, metadata.Lyrics))
	}
	if metadata.Keywords != "" {
		entries = append(entries, CreateTextDataBox(TypeKEYW, metadata.Keywords))
	}
	if metadata.Location != "" {
		entries = append(entries, CreateTextDataBox(TypeLOCI, metadata.Location))
	}

	// Copyright (special format)
	if metadata.Copyright != "" {
		entries = append(entries, CreateCopyrightBox([3]byte{'u', 'n', 'd'}, metadata.Copyright))
	}

	// Custom metadata
	for key, value := range metadata.Custom {
		entries = append(entries, CreateCustomMetadataBox("live.m7s.custom", key, []byte(value)))
	}

	return entries
}

// Helper function to create current date string
func GetCurrentDateString() string {
	return time.Now().Format("2006-01-02")
}

func init() {
	RegisterBox[*TextDataBox](TypeTITL, TypeART, TypeALB, TypeDAY, TypeCMT, TypeGEN, TypeENCO, TypeWRT, TypePRD, TypePRF, TypeGRP, TypeLYR, TypeKEYW, TypeLOCI, TypeRTNG)
	RegisterBox[*MetadataDataBox](f("data"))
	RegisterBox[*CopyrightBox](TypeCPRT)
	RegisterBox[*CustomMetadataBox](TypeMETA_CUST)
}
