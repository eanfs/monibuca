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

// WHIP push steps definition
var webrtcPushSteps = []pkg.StepDef{
	{Name: pkg.StepPublish, Description: "Publishing stream"},
	{Name: StepWebRTCInit, Description: "Initializing WebRTC connection"},
	{Name: StepOfferCreate, Description: "Creating WebRTC offer"},
	{Name: StepSessionCreate, Description: "Creating session with server"},
	{Name: StepTrackSetup, Description: "Setting up media tracks"},
	{Name: StepNegotiation, Description: "Completing WebRTC negotiation"},
	{Name: pkg.StepStreaming, Description: "Pushing media stream"},
}

// WHIPClient is a client that pushes media to the server
type WHIPClient struct {
	Client
	pushCtx m7s.PushJob
}

func (c *WHIPClient) GetPushJob() *m7s.PushJob {
	return &c.pushCtx
}

func (c *WHIPClient) Start() (err error) {
	err = c.pushCtx.Subscribe()
	if err != nil {
		return
	}
	c.Subscriber = c.pushCtx.Subscriber
	c.Info("Initializing WHIP WebRTC connection")
	err = c.Client.Start()
	if err != nil {
		return
	}

	c.Info("Creating WebRTC offer")
	var sdpBody SDPBody
	sdpBody.SessionDescription, err = c.GetOffer()
	if err != nil {
		return
	}

	// Send offer to WHIP endpoint
	c.Info("Sending offer to WHIP endpoint", "url", c.pushCtx.RemoteURL)
	c.Debug("sdp", sdpBody.SessionDescription.SDP)
	var res *http.Response
	res, err = http.DefaultClient.Post(c.pushCtx.RemoteURL, "application/sdp", strings.NewReader(sdpBody.SessionDescription.SDP))
	if err != nil {
		return
	}
	var body util.Buffer
	io.Copy(&body, res.Body)

	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		err = errors.New(res.Status + string(body))
		return
	}

	// Parse answer from server
	c.Info("Processing WHIP answer from server")
	var sd SessionDescription
	sd.Type = SDPTypeAnswer

	sd.SDP = string(body)
	if err = c.SetRemoteDescription(sd); err != nil {
		return
	}

	c.Info("WHIP negotiation completed, ready to push media")
	return
}
