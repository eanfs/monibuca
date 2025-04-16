package webtransport

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"m7s.live/v5"
	"m7s.live/v5/pkg/config"
	flv "m7s.live/v5/plugin/flv/pkg"
)

var (
	_ = m7s.InstallPlugin[WebTransportPlugin]()
)

type WebTransportPlugin struct {
	m7s.Plugin
	ListenAddr     string   `default:":4433" desc:"监听地址"`
	CertFile       string   `desc:"证书文件路径"`
	KeyFile        string   `desc:"密钥文件路径"`
	AllowedOrigins []string `desc:"允许的来源域名列表"`
}

func (p *WebTransportPlugin) OnInit() (err error) {
	// Create a new HTTP mux for WebTransport
	mux := http.NewServeMux()

	// Register the WebTransport handlers
	mux.HandleFunc("/webtransport/play/", p.handlePlay)
	mux.HandleFunc("/webtransport/push/", p.handlePush)

	// Start the WebTransport server
	server := &Server{
		Handler:        mux,
		ListenAddr:     p.ListenAddr,
		TLSCert:        CertFile{Path: p.CertFile, Data: config.LocalCert},
		TLSKey:         CertFile{Path: p.KeyFile, Data: config.LocalKey},
		AllowedOrigins: p.AllowedOrigins,
	}

	// Run the server in a goroutine
	go func() {
		if err := server.Run(p.Context); err != nil {
			p.Error("WebTransport server error", "error", err)
		}
	}()

	// Set the play and push addresses for the plugin
	_, port, _ := strings.Cut(p.ListenAddr, ":")
	p.PlayAddr = append(p.PlayAddr, fmt.Sprintf("https://{hostName}:%s/webtransport/play", port))
	p.PushAddr = append(p.PushAddr, fmt.Sprintf("https://{hostName}:%s/webtransport/push", port))

	return nil
}

func (p *WebTransportPlugin) handlePlay(w http.ResponseWriter, r *http.Request) {
	// Extract the stream path from the URL
	streamPath := strings.TrimPrefix(r.URL.Path, "/webtransport/play/")
	if streamPath == "" {
		http.Error(w, "Stream path is required", http.StatusBadRequest)
		return
	}

	// The actual WebTransport session will be handled by the Server.handleSession method
	// This function is registered as an HTTP handler, but the actual WebTransport
	// connection is established through the QUIC protocol

	// Check if the request body is a WebTransport session
	session, ok := r.Body.(*Session)
	if !ok {
		http.Error(w, "Not a WebTransport session", http.StatusBadRequest)
		return
	}
	sub, err := p.Subscribe(r.Context(), streamPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sub.RemoteAddr = r.RemoteAddr
	// Create a WebTransport subscriber
	// Accept the WebTransport session
	session.AcceptSession()

	// Create a Live FLV handler
	live := &flv.Live{Subscriber: sub}

	// Set up the FLV tag writer
	live.WriteFlvTag = func(buffers net.Buffers) (err error) {
		_, err = buffers.WriteTo(session)
		return
	}
	live.Run()
}

func (p *WebTransportPlugin) handlePush(w http.ResponseWriter, r *http.Request) {
	// Extract the stream path from the URL
	streamPath := strings.TrimPrefix(r.URL.Path, "/webtransport/push/")
	if streamPath == "" {
		http.Error(w, "Stream path is required", http.StatusBadRequest)
		return
	}

	// Check if the request body is a WebTransport session
	session, ok := r.Body.(*Session)
	if !ok {
		http.Error(w, "Not a WebTransport session", http.StatusBadRequest)
		return
	}
	// Accept the WebTransport session
	session.AcceptSession()

	var flvPuller flv.Puller
	flvPuller.ReadCloser = session
	var pubConf = p.GetCommonConf().Publish
	job := flvPuller.GetPullJob().Init(&flvPuller, &p.Plugin, streamPath, config.Pull{}, &pubConf)
	p.AddTask(job)
	job.WaitStopped()
}
