package plugin_logrotate

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/phsym/console-slog"
	"google.golang.org/protobuf/types/known/emptypb"
	"m7s.live/v5/pkg/util"
	"m7s.live/v5/plugin/logrotate/pb"
)

func (h *LogRotatePlugin) List(context.Context, *emptypb.Empty) (*pb.ResponseFileInfo, error) {
	dir, err := os.Open(h.Path)
	if err == nil {
		var files []os.FileInfo
		if files, err = dir.Readdir(0); err == nil {
			var fileInfos []*pb.FileInfo
			for _, info := range files {
				fileInfos = append(fileInfos, &pb.FileInfo{
					Name: info.Name(), Size: info.Size(),
				})
			}
			return &pb.ResponseFileInfo{Data: fileInfos}, nil
		}
	}
	return nil, err
}

func (h *LogRotatePlugin) Get(_ context.Context, req *pb.RequestFileInfo) (res *pb.ResponseOpen, err error) {
	file, err1 := os.Open(filepath.Join(h.Path, req.FileName))
	if err1 == nil {
		defer file.Close()
		res = &pb.ResponseOpen{}
		content, err2 := io.ReadAll(file)
		if err2 == nil {
			res.Data = string(content)
		} else {
			err = err2
		}
	} else {
		err = err1
	}
	return
}

func (h *LogRotatePlugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {

}

// currentLogPath 返回 rotoslog 正在写入的当前日志文件路径。文件名由
// FilePrefix + CurrentFileSuffix + FileExt 组成；这里必须跟随配置前缀，
// 否则默认前缀( m7s- )下会去打开不存在的 current.log。
func (l *LogRotatePlugin) currentLogPath() string {
	return filepath.Join(l.Path, l.FilePrefix+currentFileSuffix+logFileExt)
}

// openCurrentLog 打开当前日志文件用于回放历史内容。打开失败必须记录日志，
// 不能静默吞掉——否则 trail 只会推新行，使用者无从判断历史为何缺失。
func (l *LogRotatePlugin) openCurrentLog() (*os.File, error) {
	path := l.currentLogPath()
	file, err := os.Open(path)
	if err != nil {
		l.Warn("open current log for trail failed", "file", path, "error", err)
		return nil, err
	}
	return file, nil
}

func (l *LogRotatePlugin) API_trail(w http.ResponseWriter, r *http.Request) {
	util.NewSSE(w, r.Context(), func(sse *util.SSE) {
		if file, err := l.openCurrentLog(); err == nil {
			io.Copy(sse, file)
			file.Close()
		}
		h := console.NewHandler(sse, &console.HandlerOptions{NoColor: true})
		l.Server.LogHandler.Add(h)
		<-r.Context().Done()
		l.Server.LogHandler.Remove(h)
	})
}
