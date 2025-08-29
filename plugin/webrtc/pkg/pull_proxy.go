package webrtc

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
	"m7s.live/v5"
	"m7s.live/v5/pkg/util"
)

func NewPullProxy() m7s.IPullProxy {
	return &PullProxy{}
}

type PullProxy struct {
	Client
	m7s.BasePullProxy
}

func (p *PullProxy) Start() (err error) {
	err = p.Client.Start()
	if err != nil {
		return
	}
	p.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		p.Info("Connection State has changed:" + state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			p.ChangeStatus(m7s.PullProxyStatusOnline)
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			p.ChangeStatus(m7s.PullProxyStatusOffline)
			if !p.IsStopped() {
				time.AfterFunc(p.CheckInterval, func() {
					p.Start()
				})
			}
		}
	})
	var sdpBody SDPBody
	sdpBody.SessionDescription, err = p.GetOffer()
	if err != nil {
		return
	}

	var res *http.Response
	res, err = http.DefaultClient.Post(p.BasePullProxy.URL, "application/sdp", strings.NewReader(sdpBody.SessionDescription.SDP))
	if err != nil {
		return
	}
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		err = errors.New(res.Status)
		return
	}
	var sd webrtc.SessionDescription
	sd.Type = webrtc.SDPTypeAnswer
	var body util.Buffer
	io.Copy(&body, res.Body)
	sd.SDP = string(body)
	if err = p.SetRemoteDescription(sd); err != nil {
		return
	}
	return
}
