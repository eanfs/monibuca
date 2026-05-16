package storage

import "os"

// TempFileFinalizer 是 storage.File 的可选能力。实现者可直接接管调用方
// 已经写好的一个完整本地文件作为自身内容，避免再做一次全量拷贝。
//
// 背景：MP4 录制结束时 trailer 重写会把 moov 移到文件头，先把 [ftyp][moov][mdat]
// 写进一个临时文件，再整体覆盖回目标文件——媒体数据被写了两遍盘。临时文件本身
// 已是完整的 moov-first MP4，通过本接口直接移交，可省掉第二遍全量写入。
//
// 调用方先把完整内容写到一个本地临时文件，然后调用 FinalizeFromTemp，再调用
// File.Close() 完成最终持久化。
type TempFileFinalizer interface {
	// FinalizeFromTemp 让本 File 以 srcPath 指向的完整本地文件作为其最终内容。
	// 成功返回后 srcPath 的所有权移交给实现方，调用方不得再删除或写入它；
	// 失败时 srcPath 仍归调用方所有。
	// 调用之后再调用 File.Close() 完成最终持久化：
	//   - 对象存储后端（S3/OSS/COS）：上传该文件
	//   - 本地后端：文件已 rename 到目标路径，Close 仅关闭句柄
	FinalizeFromTemp(srcPath string) error
}

// adoptUploadTempFile 供对象存储后端（S3/OSS/COS）的 FinalizeFromTemp 复用：
// 关闭并删除旧的（通常为空的）内部临时文件，然后以读写方式打开 srcPath 接管它。
// 返回打开的文件句柄；失败时 srcPath 不被接管（调用方仍负责清理），旧文件已被关闭并删除。
func adoptUploadTempFile(old *os.File, oldPath, srcPath string) (*os.File, error) {
	if old != nil {
		old.Close()
	}
	if oldPath != "" && oldPath != srcPath {
		os.Remove(oldPath)
	}
	return os.OpenFile(srcPath, os.O_RDWR, 0644)
}
