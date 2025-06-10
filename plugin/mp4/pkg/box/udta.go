package box

import (
	"bytes"
	"io"
)

// User Data Box (udta)
// This box contains objects that declare user information about the containing box and its data.
type UserDataBox struct {
	BaseBox
	Entries []IBox
}

// Create a new User Data Box
func CreateUserDataBox(entries ...IBox) *UserDataBox {
	size := uint32(BasicBoxLen)
	for _, entry := range entries {
		size += uint32(entry.Size())
	}
	return &UserDataBox{
		BaseBox: BaseBox{
			typ:  TypeUDTA,
			size: size,
		},
		Entries: entries,
	}
}

// WriteTo writes the UserDataBox to the given writer
func (box *UserDataBox) WriteTo(w io.Writer) (n int64, err error) {
	return WriteTo(w, box.Entries...)
}

// Unmarshal parses the given buffer into a UserDataBox
func (box *UserDataBox) Unmarshal(buf []byte) (IBox, error) {
	r := bytes.NewReader(buf)
	for {
		b, err := ReadFrom(r)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		box.Entries = append(box.Entries, b)
	}
	return box, nil
}

func init() {
	RegisterBox[*UserDataBox](TypeUDTA)
}
