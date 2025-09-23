package webrtc

import (
	"errors"
	"io"
	"net/http"
	"strings"

	. "github.com/pion/webrtc/v4"
	"m7s.live/v5"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/util"
)

// WHEPClient is a client that pulls media from the server
type WHEPClient struct {
	Client
	pullCtx m7s.PullJob
}

func (c *WHEPClient) GetPullJob() *m7s.PullJob {
	return &c.pullCtx
}

func (c *WHEPClient) Start() (err error) {
	err = c.pullCtx.Publish()
	if err != nil {
		return
	}
	c.Publisher = c.pullCtx.Publisher
	c.pullCtx.GoToStepConst(StepWebRTCInit)
	err = c.Client.Start()
	if err != nil {
		return
	}
	// u, _ := url.Parse(c.pullCtx.RemoteURL)
	// c.ApiBase, _, _ = strings.Cut(c.pullCtx.RemoteURL, "?")
	if c.pullCtx.PublishConfig.PubVideo {
		var transeiver *RTPTransceiver
		transeiver, err = c.AddTransceiverFromKind(RTPCodecTypeVideo, RTPTransceiverInit{
			Direction: RTPTransceiverDirectionRecvonly,
		})
		if err != nil {
			return
		}
		c.Info("webrtc add video transceiver", "transceiver", transeiver.Mid())
	}

	if c.pullCtx.PublishConfig.PubAudio {
		var transeiver *RTPTransceiver
		transeiver, err = c.AddTransceiverFromKind(RTPCodecTypeAudio, RTPTransceiverInit{
			Direction: RTPTransceiverDirectionRecvonly,
		})
		if err != nil {
			return
		}
		c.Info("webrtc add audio transceiver", "transceiver", transeiver.Mid())
	}

	c.pullCtx.GoToStepConst(StepOfferCreate)
	var sdpBody SDPBody
	sdpBody.SessionDescription, err = c.GetOffer()
	if err != nil {
		return
	}

	c.pullCtx.GoToStepConst(StepSessionCreate)
	var res *http.Response
	res, err = http.DefaultClient.Post(c.pullCtx.RemoteURL, "application/sdp", strings.NewReader(sdpBody.SessionDescription.SDP))
	if err != nil {
		return
	}
	c.pullCtx.GoToStepConst(StepNegotiation)
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		err = errors.New(res.Status)
		return
	}
	var sd SessionDescription
	sd.Type = SDPTypeAnswer
	var body util.Buffer
	io.Copy(&body, res.Body)
	sd.SDP = string(body)
	if err = c.SetRemoteDescription(sd); err != nil {
		return
	}
	c.pullCtx.GoToStepConst(pkg.StepStreaming)
	return
}
