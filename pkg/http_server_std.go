//go:build !fasthttp

package pkg

import (
	"crypto/tls"
	"log/slog"
	"net/http"

	"github.com/langhuihui/gotask"
	"m7s.live/v5/pkg/config"
)

func CreateHTTPWork(conf *config.HTTP, logger *slog.Logger) *ListenHTTPWork {
	ret := &ListenHTTPWork{HTTP: conf}
	ret.Logger = logger.With("addr", conf.ListenAddr)
	return ret
}

func CreateHTTPSWork(conf *config.HTTP, logger *slog.Logger) *ListenHTTPSWork {
	ret := &ListenHTTPSWork{ListenHTTPWork{HTTP: conf}}
	ret.Logger = logger.With("addr", conf.ListenAddrTLS)
	return ret
}

type ListenHTTPWork struct {
	task.Task
	*config.HTTP
	*http.Server
}

func (task *ListenHTTPWork) Start() (err error) {
	task.Server = &http.Server{
		Addr:         task.ListenAddr,
		ReadTimeout:  task.HTTP.ReadTimeout,
		WriteTimeout: task.HTTP.WriteTimeout,
		IdleTimeout:  task.HTTP.IdleTimeout,
		Handler:      task.GetHandler(task.Logger),
	}
	return
}

func (task *ListenHTTPWork) Go() error {
	task.Info("listen http")
	return task.Server.ListenAndServe()
}

func (task *ListenHTTPWork) Dispose() {
	task.Info("http server stop")
	task.Server.Close()
}

type ListenHTTPSWork struct {
	ListenHTTPWork
}

func (task *ListenHTTPSWork) Start() (err error) {
	cer, _ := tls.X509KeyPair(config.LocalCert, config.LocalKey)
	task.Server = &http.Server{
		Addr:         task.HTTP.ListenAddrTLS,
		ReadTimeout:  task.HTTP.ReadTimeout,
		WriteTimeout: task.HTTP.WriteTimeout,
		IdleTimeout:  task.HTTP.IdleTimeout,
		Handler:      task.HTTP.GetHandler(task.Logger),
		TLSConfig: &tls.Config{
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
		},
	}
	return
}

func (task *ListenHTTPSWork) Go() error {
	task.Info("listen https")
	return task.Server.ListenAndServeTLS(task.HTTP.CertFile, task.HTTP.KeyFile)
}
