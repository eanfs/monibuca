package plugin_hls

import (
	"embed"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"m7s.live/v5/pkg/util"

	"m7s.live/v5"
	hls "m7s.live/v5/plugin/hls/pkg"
)

var _ = m7s.InstallPlugin[HLSPlugin](hls.NewPuller, hls.NewTransform)

//go:embed hls.js
var hls_js embed.FS

type HLSPlugin struct {
	m7s.Plugin
}

func (p *HLSPlugin) OnInit() (err error) {
	_, port, _ := strings.Cut(p.GetCommonConf().HTTP.ListenAddr, ":")
	if port == "80" {
		p.PlayAddr = append(p.PlayAddr, "http://{hostName}/hls/{streamPath}.m3u8")
	} else if port != "" {
		p.PlayAddr = append(p.PlayAddr, fmt.Sprintf("http://{hostName}:%s/hls/{streamPath}.m3u8", port))
	}
	_, port, _ = strings.Cut(p.GetCommonConf().HTTP.ListenAddrTLS, ":")
	if port == "443" {
		p.PlayAddr = append(p.PlayAddr, "https://{hostName}/hls/{streamPath}.m3u8")
	} else if port != "" {
		p.PlayAddr = append(p.PlayAddr, fmt.Sprintf("https://{hostName}:%s/hls/{streamPath}.m3u8", port))
	}
	return
}

func (p *HLSPlugin) OnPullProxyAdd(pullProxy *m7s.PullProxy) any {
	d := &m7s.HTTPPullProxy{}
	d.PullProxy = pullProxy
	d.Plugin = &p.Plugin
	return d
}

func (config *HLSPlugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fileName := strings.TrimPrefix(r.URL.Path, "/")
	query := r.URL.Query()
	waitTimeout, err := time.ParseDuration(query.Get("timeout"))
	if err == nil {
		config.Debug("request", "fileName", fileName, "timeout", waitTimeout)
	} else {
		waitTimeout = time.Second * 10
	}
	waitStart := time.Now()
	if strings.HasSuffix(r.URL.Path, ".m3u8") {
		w.Header().Add("Content-Type", "application/vnd.apple.mpegurl")
		streamPath := strings.TrimSuffix(fileName, ".m3u8")
		for {
			if v, ok := hls.MemoryM3u8.Load(streamPath); ok {
				w.Write([]byte(v.(string)))
				return
			}
			if waitTimeout > 0 && time.Since(waitStart) < waitTimeout {
				config.Server.OnSubscribe(streamPath, r.URL.Query())
				time.Sleep(time.Second)
				continue
			} else {
				break
			}
		}
		// 		w.Write([]byte(fmt.Sprintf(`#EXTM3U
		// #EXT-X-VERSION:3
		// #EXT-X-MEDIA-SEQUENCE:%d
		// #EXT-X-TARGETDURATION:%d
		// #EXT-X-DISCONTINUITY-SEQUENCE:%d
		// #EXT-X-DISCONTINUITY
		// #EXTINF:%.3f,
		// default.ts`, defaultSeq, int(math.Ceil(config.DefaultTSDuration.Seconds())), defaultSeq, config.DefaultTSDuration.Seconds())))
	} else if strings.HasSuffix(r.URL.Path, ".ts") {
		w.Header().Add("Content-Type", "video/mp2t") //video/mp2t
		streamPath := path.Dir(fileName)
		for {
			tsData, ok := hls.MemoryTs.Load(streamPath)
			if !ok {
				tsData, ok = hls.MemoryTs.Load(path.Dir(streamPath))
				if !ok {
					if waitTimeout > 0 && time.Since(waitStart) < waitTimeout {
						time.Sleep(time.Second)
						continue
					} else {
						// w.Write(defaultTS)
						return
					}
				}
			}
			for {
				if tsData, ok := tsData.(hls.TsCacher).GetTs(fileName); ok {
					switch v := tsData.(type) {
					case *hls.TsInMemory:
						v.WriteTo(w)
					case util.Buffer:
						w.Write(v)
					}
					return
				} else {
					if waitTimeout > 0 && time.Since(waitStart) < waitTimeout {
						time.Sleep(time.Second)
						continue
					} else {
						// w.Write(defaultTS)
						return
					}
				}
			}
		}
	} else {
		f, err := hls_js.ReadFile("hls.js/" + fileName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			w.Write(f)
		}
		// if file, err := hls_js.Open(fileName); err == nil {
		// 	defer file.Close()
		// 	if info, err := file.Stat(); err == nil {
		// 		http.ServeContent(w, r, fileName, info.ModTime(), file)
		// 	}
		// } else {
		// 	http.NotFound(w, r)
		// }
	}
}
