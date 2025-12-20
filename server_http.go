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

// RedirectAdvisorV2 is an optional extension that provides request scheme context (http/https),
// allowing advisors to choose the correct target port.
type RedirectAdvisorV2 interface {
	GetRedirectTargetV2(protocol, streamPath, currentHost, scheme string) (targetHost string, statusCode int, ok bool)
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
// The prefix parameter is the plugin name (e.g., "flv", "mp4", "hls", "webrtc") used to restore the path prefix.
// The redirectPath parameter is the stream path without prefix, used for matching redirect rules.
// This function automatically restores the path prefix (e.g., /flv/, /mp4/) for building the redirect URL.
func (s *Server) RedirectIfNeeded(w http.ResponseWriter, r *http.Request, prefix, redirectPath string) bool {
	if s == nil {
		return false
	}
	advisor := s.GetRedirectAdvisor()
	if advisor == nil {
		return false
	}

	// Save current path and restore it with the prefix for building redirect URL
	currentPath := r.URL.Path
	pathRestored := false

	// Restore path with prefix if needed
	if prefix != "" {
		pathPrefix := "/" + prefix + "/"
		if !strings.HasPrefix(currentPath, pathPrefix) {
			r.URL.Path = pathPrefix + redirectPath
			pathRestored = true
		}
	}

	// Restore original path after redirect (if it was modified)
	defer func() {
		if pathRestored {
			r.URL.Path = currentPath
		}
	}()

	var targetHost string
	var statusCode int
	var ok bool
	if adv2, ok2 := advisor.(RedirectAdvisorV2); ok2 {
		targetHost, statusCode, ok = adv2.GetRedirectTargetV2(prefix, redirectPath, r.Host, requestScheme(r))
	} else {
		targetHost, statusCode, ok = advisor.GetRedirectTarget(prefix, redirectPath, r.Host)
	}
	if !ok || targetHost == "" {
		return false
	}
	if statusCode == 0 {
		statusCode = http.StatusFound
	}
	// Use r.URL.Path (which has been restored with prefix) to build redirect URL
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
