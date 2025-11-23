package m7s

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"m7s.live/v5/pkg/format"
	"m7s.live/v5/pkg/util"
)

type RedirectAdvisor interface {
	GetRedirectTarget(protocol, streamPath, currentHost string) (targetHost string, statusCode int, ok bool)
}

func (s *Server) GetRedirectAdvisor() RedirectAdvisor {
	if s == nil {
		return nil
	}
	s.redirectOnce.Do(func() {
		s.Plugins.Range(func(plugin *Plugin) bool {
			if advisor, ok := plugin.GetHandler().(RedirectAdvisor); ok {
				s.redirectAdvisor = advisor
				return false
			}
			return true
		})
	})
	return s.redirectAdvisor
}

// RedirectIfNeeded evaluates redirect advice for HTTP-based protocols and issues redirects when appropriate.
func (s *Server) RedirectIfNeeded(w http.ResponseWriter, r *http.Request, protocol, redirectPath string) bool {
	if s == nil {
		return false
	}
	advisor := s.GetRedirectAdvisor()
	if advisor == nil {
		return false
	}
	targetHost, statusCode, ok := advisor.GetRedirectTarget(protocol, redirectPath, r.Host)
	if !ok || targetHost == "" {
		return false
	}
	if statusCode == 0 {
		statusCode = http.StatusFound
	}
	redirectURL := buildRedirectURL(r, targetHost)
	http.Redirect(w, r, redirectURL, statusCode)
	return true
}

func buildRedirectURL(r *http.Request, host string) string {
	scheme := requestScheme(r)
	if isWebSocketRequest(r) {
		switch scheme {
		case "https":
			scheme = "wss"
		case "http":
			scheme = "ws"
		}
	}
	target := &url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}
	return target.String()
}

func requestScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func isWebSocketRequest(r *http.Request) bool {
	connection := strings.ToLower(r.Header.Get("Connection"))
	return strings.Contains(connection, "upgrade") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Check for location-based forwarding first
	if s.Location != nil {
		for pattern, target := range s.Location {
			if pattern.MatchString(r.URL.Path) {
				// Rewrite the URL path and handle locally
				r.URL.Path = pattern.ReplaceAllString(r.URL.Path, target)
				// Forward to local handler
				s.config.HTTP.GetHandler(s.Logger).ServeHTTP(w, r)
				return
			}
		}
	}

	// 检查 admin.zip 是否需要重新加载
	now := time.Now()
	if now.Sub(s.Admin.lastCheckTime) > checkInterval {
		if info, err := os.Stat(s.Admin.FilePath); err == nil && info.ModTime() != s.Admin.zipLastModTime {
			s.Info("admin.zip changed, reloading...")
			s.loadAdminZip()
		}
		s.Admin.lastCheckTime = now
	}

	if s.Admin.zipReader != nil {
		// Handle root path redirect to HomePage
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin/#/"+s.Admin.HomePage, http.StatusFound)
			return
		}

		// For .map files, set correct content-type before serving
		if strings.HasSuffix(r.URL.Path, ".map") {
			filePath := strings.TrimPrefix(r.URL.Path, "/admin/")
			file, err := s.Admin.zipReader.Open(filePath)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer file.Close()
			w.Header().Set("Content-Type", "application/json")
			io.Copy(w, file)
			return
		}

		http.ServeFileFS(w, r, s.Admin.zipReader, strings.TrimPrefix(r.URL.Path, "/admin"))
		return
	}
	if r.URL.Path == "/favicon.ico" {
		http.ServeFile(w, r, "favicon.ico")
		return
	}
	_, _ = fmt.Fprintf(w, "visit:%s\nMonibuca Engine %s StartTime:%s\n", r.URL.Path, Version, s.StartTime)
	for plugin := range s.Plugins.Range {
		_, _ = fmt.Fprintf(w, "Plugin %s Version:%s\n", plugin.Meta.Name, plugin.Meta.Version)
	}
	for _, api := range s.apiList {
		_, _ = fmt.Fprintf(w, "%s\n", api)
	}
}

func (s *Server) annexB(w http.ResponseWriter, r *http.Request) {
	streamPath := r.PathValue("streamPath")

	if r.URL.RawQuery != "" {
		streamPath += "?" + r.URL.RawQuery
	}
	var conf = s.config.Subscribe
	conf.SubType = SubscribeTypeServer
	conf.SubAudio = false
	suber, err := s.SubscribeWithConfig(r.Context(), streamPath, conf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var ctx util.HTTP_WS_Writer
	ctx.Conn, err = suber.CheckWebSocket(w, r)
	if err != nil {
		return
	}
	ctx.WriteTimeout = s.GetCommonConf().WriteTimeout
	ctx.ContentType = "application/octet-stream"
	ctx.ServeHTTP(w, r)

	PlayBlock(suber, func(frame *format.RawAudio) (err error) {
		return nil
	}, func(frame *format.AnnexB) (err error) {
		_, err = frame.WriteTo(&ctx)
		if err != nil {
			return
		}
		return ctx.Flush()
	})
}

