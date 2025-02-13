package box

import (
	"bytes"
	"io"
)

type (
	MoovBox struct {
		BaseBox
		Tracks []*TrakBox
		// UDTA   *UdtaBody
		MVHD *MovieHeaderBox
		MVEX *MovieExtendsBox
	}

	EdtsBox struct {
		BaseBox
		Elst *EditListBox
	}
)

func (m *MoovBox) WriteTo(w io.Writer) (n int64, err error) {
	var boxes []IBox
	boxes = append(boxes, m.MVHD)
	for _, track := range m.Tracks {
		boxes = append(boxes, track)
	}
	return WriteTo(w, boxes...)
}

func (m *MoovBox) Unmarshal(buf []byte) (IBox, error) {
	r := bytes.NewReader(buf)
	for {
		b, err := ReadFrom(r)
		if err != nil {
			return m, err
		}
		switch box := b.(type) {
		case *TrakBox:
			m.Tracks = append(m.Tracks, box)
		case *MovieHeaderBox:
			m.MVHD = box
		case *MovieExtendsBox:
			m.MVEX = box
		}
	}
}

func (e *EdtsBox) WriteTo(w io.Writer) (n int64, err error) {
	return WriteTo(w, e.Elst)
}

func (e *EdtsBox) Unmarshal(buf []byte) (b IBox, err error) {
	r := bytes.NewReader(buf)
	for err == nil {
		b, err = ReadFrom(r)
		if err != nil {
			return e, err
		}
		switch box := b.(type) {
		case *EditListBox:
			e.Elst = box
		}
	}
	return e, err
}

func init() {
	RegisterBox[*MoovBox](TypeMOOV)
	RegisterBox[*EdtsBox](TypeEDTS)
}
