package bits

// Reader is a bit stream reader
type Reader struct {
	Data   []byte
	Offset int
}

// Skip skips n bits
func (r *Reader) Skip(n int) {
	r.Offset += n
}

// ReadBit reads a single bit
func (r *Reader) ReadBit() (uint, error) {
	if r.Offset/8 >= len(r.Data) {
		return 0, nil
	}
	b := r.Data[r.Offset/8]
	v := (b >> (7 - (r.Offset % 8))) & 0x01
	r.Offset++
	return uint(v), nil
}

// ReadExpGolomb reads an Exp-Golomb code
func (r *Reader) ReadExpGolomb() (uint, error) {
	leadingZeroBits := 0
	for {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		leadingZeroBits++
	}

	result := uint(1<<leadingZeroBits) - 1
	for i := 0; i < leadingZeroBits; i++ {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		result = (result << 1) | uint(b)
	}
	return result, nil
}

// ReadSE reads a signed Exp-Golomb code
func (r *Reader) ReadSE() (int, error) {
	val, err := r.ReadExpGolomb()
	if err != nil {
		return 0, err
	}
	sign := ((val & 0x01) << 1) - 1
	val = ((val >> 1) + (val & 0x01)) * uint(sign)
	return int(val), nil
}
