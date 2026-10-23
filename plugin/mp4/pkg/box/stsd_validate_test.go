package box

import (
	"encoding/binary"
	"errors"
	"testing"
)

// stsdUnmarshalPayload 造 STSDBox.Unmarshal 的输入：entry_count + 若干 entry。
// ReadFrom 已经把 version/flags 剥掉，Unmarshal 只看 entry_count 起的内容。
func stsdUnmarshalPayload(entryCount uint32, entries ...[]byte) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, entryCount)
	for _, entry := range entries {
		buf = append(buf, entry...)
	}
	return buf
}

// 未注册类型的 sample entry 就足够：ReadFrom 会按声明长度返回 *BaseBox，
// 不会走 Visual/AudioSampleEntry 的字段解析（空负载会在那里 panic）。
func stsdTestEntry(typ string) []byte {
	return append([]byte{0, 0, 0, 8}, []byte(typ)...)
}

// stsd 是不可信输入：entry_count 与实际条目数不一致、或 entry_count==0 都是
// 结构性损坏。静默接受会让恢复路径拿到一条没有 sample entry 的轨道并写出
// 不可播的 moov，因此必须显式返回 ErrSTSDCorrupt。
func TestSTSDUnmarshalRejectsStructuralDamage(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"zero_entries", stsdUnmarshalPayload(0)},
		{"count_exceeds_entries", stsdUnmarshalPayload(2, stsdTestEntry("za01"))},
		{"entries_exceed_count", stsdUnmarshalPayload(1, stsdTestEntry("za01"), stsdTestEntry("za02"))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stsd := &STSDBox{}
			got, err := stsd.Unmarshal(c.payload)
			if err == nil {
				t.Fatalf("expected structural damage error, got %T entries=%d", got, len(stsd.Entries))
			}
			if !errors.Is(err, ErrSTSDCorrupt) {
				t.Fatalf("error = %v, want ErrSTSDCorrupt", err)
			}
		})
	}
}

// 合法 stsd（entry_count 与条目数一致）必须照旧解析。
func TestSTSDUnmarshalAcceptsConsistentCount(t *testing.T) {
	stsd := &STSDBox{}
	got, err := stsd.Unmarshal(stsdUnmarshalPayload(2, stsdTestEntry("za01"), stsdTestEntry("za02")))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != stsd || len(stsd.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(stsd.Entries))
	}
	if stsd.Entries[0].Type().String() != "za01" || stsd.Entries[1].Type().String() != "za02" {
		t.Fatalf("entries = %s/%s", stsd.Entries[0].Type().String(), stsd.Entries[1].Type().String())
	}
}
