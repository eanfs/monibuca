package config

import (
	"log/slog"
	"net/http"

	"m7s.live/v5/pkg/util"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	"time"
)

type Middleware func(string, http.Handler) http.Handler
type HTTP struct {
	ListenAddr    string        `desc:"监听地址"`
	ListenAddrTLS string        `desc:"监听地址HTTPS"`
	CertFile      string        `desc:"HTTPS证书文件"`
	KeyFile       string        `desc:"HTTPS密钥文件"`
	CORS          bool          `default:"true" desc:"是否自动添加CORS头"` //是否自动添加CORS头
	UserName      string        `desc:"基本身份认证用户名"`
	Password      string        `desc:"基本身份认证密码"`
	ReadTimeout   time.Duration `desc:"读取超时"`
	WriteTimeout  time.Duration `desc:"写入超时"`
	IdleTimeout   time.Duration `desc:"空闲超时"`
	mux           *http.ServeMux
	grpcMux       *runtime.ServeMux
	middlewares   []Middleware
}

func (config *HTTP) logHandler(logger *slog.Logger, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		logger.Debug("visit", "path", r.URL.String(), "remote", r.RemoteAddr)
		handler.ServeHTTP(rw, r)
	})
}

func (config *HTTP) GetHandler(logger *slog.Logger) (h http.Handler) {
	if config.grpcMux != nil {
		h = config.grpcMux
		if logger != nil {
			h = config.logHandler(logger, h)
		}
		if config.CORS {
			h = util.CORS(h)
		}
		if config.UserName != "" && config.Password != "" {
			h = util.BasicAuth(config.UserName, config.Password, h)
		}
		return
	}
	return config.mux
}

func (config *HTTP) CreateHttpMux() http.Handler {
	config.mux = http.NewServeMux()
	return config.mux
}

func (config *HTTP) GetGRPCMux() *runtime.ServeMux {
	return config.grpcMux
}

func (config *HTTP) SetMux(mux *runtime.ServeMux) {
	config.grpcMux = mux
	handler := func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		config.mux.ServeHTTP(w, r)
	}
	mux.HandlePath("GET", "/", handler)
	mux.HandlePath("POST", "/", handler)
}

func (config *HTTP) AddMiddleware(middleware Middleware) {
	config.middlewares = append(config.middlewares, middleware)
}

func (config *HTTP) Handle(path string, f http.Handler, last bool) {
	if config.mux == nil {
		config.mux = http.NewServeMux()
	}
	if config.CORS {
		f = util.CORS(f)
	}
	if config.UserName != "" && config.Password != "" {
		f = util.BasicAuth(config.UserName, config.Password, f)
	}
	for _, middleware := range config.middlewares {
		f = middleware(path, f)
	}
	config.mux.Handle(path, f)
}
