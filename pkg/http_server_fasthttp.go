//go:build fasthttp

package pkg

import (
	"crypto/tls"
	"log/slog"

	"github.com/langhuihui/gotask"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"m7s.live/v5/pkg/config"
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
	cer, _ := tls.X509KeyPair(config.LocalCert, config.LocalKey)
	// 调用基类的 Start
	if err = task.ListenFastHTTPWork.Start(); err != nil {
		return err
	}
	task.server.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cer},
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			//tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			//tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			//tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			//tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		},
	}
	return
}

func (task *ListenFastHTTPSWork) Go() error {
	task.Info("listen https fasthttp")
	return task.server.ListenAndServeTLS(task.ListenAddrTLS, task.CertFile, task.KeyFile)
}
