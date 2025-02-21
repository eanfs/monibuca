package box

import "io"

/*

aligned(8) class SampleDependencyTypeBox
 extends FullBox(‘sdtp’, version = 0, 0) {
 for (i=0; i < sample_count; i++){
 unsigned int(2) is_leading;
 unsigned int(2) sample_depends_on;
 unsigned int(2) sample_is_depended_on;
 unsigned int(2) sample_has_redundancy;
 }
}

is_leading takes one of the following four values:
0: the leading nature of this sample is unknown;
1: this sample is a leading sample that has a dependency before the referenced I-picture (and is
therefore not decodable);
2: this sample is not a leading sample;
3: this sample is a leading sample that has no dependency before the referenced I-picture (and is
therefore decodable);
sample_depends_on takes one of the following four values:
0: the dependency of this sample is unknown;
1: this sample does depend on others (not an I picture);
2: this sample does not depend on others (I picture);
3: reserved
sample_is_depended_on takes one of the following four values:
0: the dependency of other samples on this sample is unknown;
1: other samples may depend on this one (not disposable);
2: no other sample depends on this one (disposable);
3: reserved
sample_has_redundancy takes one of the following four values:
0: it is unknown whether there is redundant coding in this sample;
1: there is redundant coding in this sample;
2: there is no redundant coding in this sample;
3: reserved

*/

type SampleDependencyTypeFlags struct {
	IsLeading     bool
	DependsOn     bool
	IsDependedOn  bool
	HasRedundancy bool
}

type SampleDependencyTypeBox struct {
	FullBox
	SampleDependencyTypeFlags
}

func CreateSampleDependencyTypeBox(flags SampleDependencyTypeFlags) *SampleDependencyTypeBox {
	return &SampleDependencyTypeBox{FullBox: FullBox{BaseBox: BaseBox{typ: TypeSDTP, size: FullBoxLen + 1}, Version: 0, Flags: [3]byte{0, 0, 0}}, SampleDependencyTypeFlags: flags}
}

func (box *SampleDependencyTypeBox) WriteTo(w io.Writer) (n int64, err error) {
	var flag byte
	if box.IsLeading {
		flag |= 1 << 6
	}
	if box.DependsOn {
		flag |= 2 << 4
	} else {
		flag |= 1 << 4
	}
	if box.IsDependedOn {
		flag |= 1 << 2
	}
	if box.HasRedundancy {
		flag |= 1
	}
	w.Write([]byte{flag})
	return 1, nil
}

func (box *SampleDependencyTypeBox) Unmarshal(buf []byte) (IBox, error) {
	box.IsLeading = buf[0]>>6&1 == 1
	box.DependsOn = buf[0]>>4&2 == 2
	box.IsDependedOn = buf[0]>>2&1 == 1
	box.HasRedundancy = buf[0]&1 == 1
	return box, nil
}
