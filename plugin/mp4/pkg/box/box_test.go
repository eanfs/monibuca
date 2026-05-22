package box

import (
	"bytes"
	"testing"
)

// TestIsNil 验证 isNil 对纯 nil 接口、typed-nil 指针、正常 box 的判定。
func TestIsNil(t *testing.T) {
	var pureNil IBox
	if !isNil(pureNil) {
		t.Error("纯 nil 接口应判为 nil")
	}
	var typedNil *ContainerBox
	if !isNil(typedNil) {
		t.Error("typed-nil *ContainerBox 应判为 nil")
	}
	if isNil(CreateContainerBox(TypeFREE)) {
		t.Error("正常 box 不应判为 nil")
	}
}

// TestCreateContainerBox_SkipsNilChildren 验证 CreateContainerBox 跳过 nil 子 box
// 且不 panic（旧实现对纯 nil 接口调 reflect.Value.IsNil() 会 panic）。
func TestCreateContainerBox_SkipsNilChildren(t *testing.T) {
	var pureNil IBox
	var typedNil *ContainerBox
	real := CreateContainerBox(TypeFREE)

	c := CreateContainerBox(TypeMOOV, pureNil, typedNil, real)
	if len(c.Children) != 1 {
		t.Fatalf("应只保留 1 个有效子 box，实际 %d", len(c.Children))
	}
}

// TestWriteTo_SkipsNil 验证 WriteTo 跳过 nil 子 box 不 panic。
func TestWriteTo_SkipsNil(t *testing.T) {
	var pureNil IBox
	var typedNil *ContainerBox
	var buf bytes.Buffer

	n, err := WriteTo(&buf, pureNil, typedNil)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != 0 {
		t.Fatalf("全 nil 子 box 应写 0 字节，实际 %d", n)
	}
}
