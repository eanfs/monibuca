package webrtc

import (
	"errors"
	"strings"

	. "github.com/pion/webrtc/v4"
	"m7s.live/v5"
	"m7s.live/v5/pkg/config"
)

const (
	DIRECTION_PULL = "pull"
	DIRECTION_PUSH = "push"
)

type PullRequest struct {
	Tracks []TrackInfo `json:"tracks"`
}

type Client struct {
	MultipleConnection
}

func (c *Client) Start() (err error) {
	var api *API
	api, err = CreateAPI(nil, SettingEngine{})
	if err != nil {
		return errors.Join(err, errors.New("create api failed"))
	}
	c.PeerConnection, err = api.NewPeerConnection(Configuration{
		ICEServers:         ICEServers,
		BundlePolicy:       BundlePolicyMaxBundle,
		ICETransportPolicy: ICETransportPolicyAll,
	})
	if err != nil {
		return
	}
	return c.MultipleConnection.Start()
}

func NewPuller(conf config.Pull) m7s.IPuller {
	if strings.HasPrefix(conf.URL, "https://rtc.live.cloudflare.com") {
		return NewCFClient(DIRECTION_PULL)
	}
	client := &WHEPClient{}
	client.pullCtx.SetProgressStepsDefs(webrtcPullSteps)
	return client
}

func NewPusher() m7s.IPusher {
	return &WHIPClient{}
}
