//go:build fasthttp

package util

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os/exec"

	"github.com/valyala/fasthttp"
	"gopkg.in/yaml.v3"
)

// 定义 SSE 常量，与 sse.go 中保持一致
var (
	// 这些变量需要在这里重新定义，因为使用构建标签后无法共享
	sseEent  = []byte("event: ")
	sseBegin = []byte("data: ")
	sseEnd   = []byte("\n\n")
)

// SSE 结构体在 fasthttp 构建模式下的实现
type SSE struct {
	Writer *bufio.Writer
	context.Context
}

func (sse *SSE) Write(data []byte) (n int, err error) {
	if err = sse.Err(); err != nil {
		return
	}
	buffers := net.Buffers{sseBegin, data, sseEnd}
	nn, err := buffers.WriteTo(sse.Writer)
	if err == nil {
		sse.Writer.Flush()
	}
	return int(nn), err
}

func (sse *SSE) WriteEvent(event string, data []byte) (err error) {
	if err = sse.Err(); err != nil {
		return
	}
	buffers := net.Buffers{sseEent, []byte(event + "\n"), sseBegin, data, sseEnd}
	_, err = buffers.WriteTo(sse.Writer)
	if err == nil {
		sse.Writer.Flush()
	}
	return
}

func NewSSE(w http.ResponseWriter, ctx context.Context, block func(sse *SSE)) (sse *SSE) {
	reqCtx := ctx.(*fasthttp.RequestCtx)
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	header.Set("Access-Control-Allow-Origin", "*")
	sse = &SSE{
		Context: ctx,
	}
	reqCtx.Response.SetBodyStreamWriter(func(w *bufio.Writer) {
		sse.Writer = w
		block(sse)
		<-ctx.Done()
	})
	return sse
}

func (sse *SSE) WriteJSON(data any) error {
	return json.NewEncoder(sse).Encode(data)
}

func (sse *SSE) WriteYAML(data any) error {
	return yaml.NewEncoder(sse).Encode(data)
}

// WriteExec 执行命令并将输出写入 SSE 流
func (sse *SSE) WriteExec(cmd *exec.Cmd) error {
	cmd.Stderr = sse
	cmd.Stdout = sse
	return cmd.Run()
}
