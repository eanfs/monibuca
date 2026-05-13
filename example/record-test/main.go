package main

import (
	"context"
	"flag"

	"m7s.live/v5"
	_ "m7s.live/v5/plugin/debug"
	_ "m7s.live/v5/plugin/flv"
	_ "m7s.live/v5/plugin/hls"
	_ "m7s.live/v5/plugin/logrotate"
	_ "m7s.live/v5/plugin/mp4"
	_ "m7s.live/v5/plugin/rtmp"
	_ "m7s.live/v5/plugin/rtsp"
	_ "m7s.live/v5/plugin/webrtc"
)

func main() {
	conf := flag.String("c", "config.yaml", "config file")
	flag.Parse()
	m7s.Run(context.Background(), *conf)
}
