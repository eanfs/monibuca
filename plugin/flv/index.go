package plugin_flv

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/util"
	"m7s.live/v5/plugin/flv/pb"
	. "m7s.live/v5/plugin/flv/pkg"
)

type FLVPlugin struct {
	pb.UnimplementedApiServer
	m7s.Plugin
	Path string
	// When enabled, WebSocket playback will create a local pull proxy instead of issuing redirects.
	ProxyOnRedirect bool `default:"false" desc:"集群下 WebSocket 播放使用本地拉流代理替代 302 重定向"`
}

const defaultConfig m7s.DefaultYaml = `publish:
  speed: 1`

var _ = m7s.InstallPlugin[FLVPlugin](m7s.PluginMeta{
	DefaultYaml:         defaultConfig,
	NewPuller:           NewPuller,
	NewRecorder:         NewRecorder,
	RegisterGRPCHandler: pb.RegisterApiHandler,
	ServiceDesc:         &pb.Api_ServiceDesc,
	NewPullProxy:        m7s.NewHTTPPullPorxy,
})

func (plugin *FLVPlugin) Start() (err error) {
	_, port, _ := strings.Cut(plugin.GetCommonConf().HTTP.ListenAddr, ":")
	if port == "80" {
		plugin.PlayAddr = append(plugin.PlayAddr, "http://{hostName}/flv/{streamPath}", "ws://{hostName}/flv/{streamPath}")
	} else if port != "" {
		plugin.PlayAddr = append(plugin.PlayAddr, fmt.Sprintf("http://{hostName}:%s/flv/{streamPath}", port), fmt.Sprintf("ws://{hostName}:%s/flv/{streamPath}", port))
	}
	_, port, _ = strings.Cut(plugin.GetCommonConf().HTTP.ListenAddrTLS, ":")
	if port == "443" {
		plugin.PlayAddr = append(plugin.PlayAddr, "https://{hostName}/flv/{streamPath}", "wss://{hostName}/flv/{streamPath}")
	} else if port != "" {
		plugin.PlayAddr = append(plugin.PlayAddr, fmt.Sprintf("https://{hostName}:%s/flv/{streamPath}", port), fmt.Sprintf("wss://{hostName}:%s/flv/{streamPath}", port))
	}
	plugin.registerJessicaRoot()
	return
}

func (plugin *FLVPlugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	redirectPath := strings.TrimPrefix(r.URL.Path, "/")
	streamPath := strings.TrimSuffix(redirectPath, ".flv")
	streamPathOnly := strings.TrimPrefix(streamPath, "flv/")
	streamPathOnly = strings.TrimPrefix(streamPathOnly, "/")

	if plugin.Server != nil {
		if isWebSocketRequest(r) && plugin.ProxyOnRedirect {
			if streamPathOnly != "" && !plugin.Server.Streams.SafeHas(streamPathOnly) {
				if advisor := plugin.Server.GetRedirectAdvisor(); advisor != nil {
					scheme := requestScheme(r)
					var target string
					if adv2, ok := advisor.(m7s.RedirectAdvisorV2); ok {
						target, _, _ = adv2.GetRedirectTargetV2("flv", streamPath, r.Host, scheme)
					} else {
						target, _, _ = advisor.GetRedirectTarget("flv", streamPath, r.Host)
					}
					if target != "" {
						plugin.ensureFLVPullProxy(streamPathOnly, streamPath, target, scheme)
					}
				}
			}
		} else if plugin.Server.RedirectIfNeeded(w, r, "flv", redirectPath) {
			plugin.Debug("redirect issued", "protocol", "http", "path", redirectPath)
			return
		}
	}
	var err error
	defer func() {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}()
	var live Live
	if r.URL.RawQuery != "" {
		streamPath += "?" + r.URL.RawQuery
	}
	live.Subscriber, err = plugin.Subscribe(r.Context(), streamPath)
	if err != nil {
		return
	}
	live.Subscriber.RemoteAddr = r.RemoteAddr

	var ctx util.HTTP_WS_Writer
	ctx.Conn, err = live.Subscriber.CheckWebSocket(w, r)
	if err != nil {
		return
	}
	ctx.WriteTimeout = plugin.GetCommonConf().WriteTimeout
	ctx.ContentType = "video/x-flv"
	ctx.ServeHTTP(w, r)
	live.WriteFlvTag = func(flv net.Buffers) (err error) {
		_, err = flv.WriteTo(&ctx)
		if err != nil {
			return
		}
		return ctx.Flush()
	}
	err = live.Run()
}

func (plugin *FLVPlugin) ensureFLVPullProxy(streamPathOnly, streamPath, target, scheme string) {
	if streamPathOnly == "" || target == "" || plugin.Server == nil {
		return
	}
	pullURL := buildFLVPullURL(target, streamPath, scheme)
	if pullURL == "" {
		return
	}
	conf := &m7s.PullProxyConfig{
		Name:        fmt.Sprintf("flv-proxy-%s", sanitizeFLVProxyName(streamPathOnly)),
		StreamPath:  streamPathOnly,
		PullOnStart: true,
		StopOnIdle:  true,
		Audio:       true,
		Type:        "flv",
	}
	conf.URL = pullURL
	pullProxy, _, err := plugin.Server.EnsurePullProxy(conf)
	if err != nil || pullProxy == nil {
		plugin.Warn("FLV proxy ensure failed", "streamPath", streamPathOnly, "pullURL", pullURL, "error", err)
		return
	}
	cfg := pullProxy.GetConfig()
	if cfg.Status == m7s.PullProxyStatusDisabled {
		plugin.Warn("FLV proxy disabled", "streamPath", streamPathOnly)
		return
	}
	if cfg.Status == m7s.PullProxyStatusOffline {
		pullProxy.ChangeStatus(m7s.PullProxyStatusOnline)
	}
}

func buildFLVPullURL(target, streamPath, scheme string) string {
	if target == "" {
		return ""
	}
	pullScheme := "http"
	switch scheme {
	case "https", "wss":
		pullScheme = "https"
	}
	pathPart, queryPart, _ := strings.Cut(streamPath, "?")
	if pathPart != "" && !strings.HasPrefix(pathPart, "/") {
		pathPart = "/" + pathPart
	}
	pathPart = "/flv" + pathPart
	u := url.URL{
		Scheme:   pullScheme,
		Host:     target,
		Path:     pathPart,
		RawQuery: queryPart,
	}
	return u.String()
}

func sanitizeFLVProxyName(streamPath string) string {
	if streamPath == "" {
		return "unknown"
	}
	return strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "_").Replace(streamPath)
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
