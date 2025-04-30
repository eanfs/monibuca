//go:build fasthttp

package pkg

import (
	"log/slog"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/task"
)

func CreateHTTPWork(conf *config.HTTP, logger *slog.Logger) *ListenFastHTTPWork {
	ret := &ListenFastHTTPWork{HTTP: conf}
	ret.Logger = logger.With("addr", conf.ListenAddr)
	return ret
}

func CreateHTTPSWork(conf *config.HTTP, logger *slog.Logger) *ListenFastHTTPSWork {
	ret := &ListenFastHTTPSWork{ListenFastHTTPWork{HTTP: conf}}
	ret.Logger = logger.With("addr", conf.ListenAddrTLS)
	return ret
}

// ListenFastHTTPWork 用于启动 FastHTTP 服务
type ListenFastHTTPWork struct {
	task.Task
	*config.HTTP
	server *fasthttp.Server
}

// 主请求处理函数
func (task *ListenFastHTTPWork) requestHandler(ctx *fasthttp.RequestCtx) {
	// 适配到标准库处理
	// fasthttpadaptor.ConvertRequest(ctx, req, false)
	// 如果有 grpcMux，通过适配器转发
	// if string(ctx.Request.Header.Peek("Accept")) == "text/event-stream" {
	// 	ctx.SetContentType("text/event-stream")
	// 	ctx.Response.Header.Set("Cache-Control", "no-cache")
	// 	ctx.Response.Header.Set("Connection", "keep-alive")
	// 	ctx.Response.Header.Set("X-Accel-Buffering", "no")
	// 	ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
	// }
	fasthttpadaptor.NewFastHTTPHandler(task.GetHandler())(ctx)
}

func (task *ListenFastHTTPWork) Start() (err error) {
	// 配置 fasthttp 服务器
	task.server = &fasthttp.Server{
		Handler:      task.requestHandler,
		ReadTimeout:  task.HTTP.ReadTimeout,
		WriteTimeout: task.HTTP.WriteTimeout,
		IdleTimeout:  task.HTTP.IdleTimeout,
		Name:         "Monibuca FastHTTP Server",
		// 启用流式响应支持
		StreamRequestBody: true,
	}
	return nil
}

func (task *ListenFastHTTPWork) Go() error {
	task.Info("listen fasthttp")
	return task.server.ListenAndServe(task.ListenAddr)
}

func (task *ListenFastHTTPWork) Dispose() {
	task.Info("fasthttp server stop")
	if task.server != nil {
		if err := task.server.Shutdown(); err != nil {
			task.Error("shutdown error", "err", err)
		}
	}
}

// ListenFastHTTPSWork 用于启动 HTTPS FastHTTP 服务
type ListenFastHTTPSWork struct {
	ListenFastHTTPWork
}

func (task *ListenFastHTTPSWork) Start() (err error) {
	// 调用基类的 Start
	if err = task.ListenFastHTTPWork.Start(); err != nil {
		return err
	}
	return nil
}

func (task *ListenFastHTTPSWork) Go() error {
	task.Info("listen https fasthttp")
	return task.server.ListenAndServeTLS(task.ListenAddrTLS, task.CertFile, task.KeyFile)
}
