package webrtc

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	. "github.com/pion/webrtc/v3"
	"m7s.live/v5"
	. "m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/task"
	"m7s.live/v5/pkg/util"
	flv "m7s.live/v5/plugin/flv/pkg"
	mrtp "m7s.live/v5/plugin/rtp/pkg"
)

type Connection struct {
	task.Task
	*PeerConnection
	SDP string
	// LocalSDP *sdp.SessionDescription
	Publisher  *m7s.Publisher
	Subscriber *m7s.Subscriber
	EnableDC   bool
	PLI        time.Duration
}

func (IO *Connection) Start() (err error) {
	if IO.Publisher != nil {
		IO.Receive()
	}
	if IO.Subscriber != nil {
		IO.Send()
	}
	IO.OnICECandidate(func(ice *ICECandidate) {
		if ice != nil {
			IO.Info(ice.ToJSON().Candidate)
		}
	})
	IO.OnConnectionStateChange(func(state PeerConnectionState) {
		IO.Info("Connection State has changed:" + state.String())
		switch state {
		case PeerConnectionStateConnected:

		case PeerConnectionStateDisconnected, PeerConnectionStateFailed, PeerConnectionStateClosed:
			IO.Stop(errors.New("connection state:" + state.String()))
		}
	})
	return
}

func (IO *Connection) GetOffer() (*SessionDescription, error) {
	offer, err := IO.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	// IO.LocalSDP, err = offer.Unmarshal()
	// if err != nil {
	// 	return "", err
	// }
	gatherComplete := GatheringCompletePromise(IO.PeerConnection)
	if err = IO.SetLocalDescription(offer); err != nil {
		return nil, err
	}
	<-gatherComplete
	return IO.LocalDescription(), nil
}

func (IO *Connection) GetAnswer() (*SessionDescription, error) {
	// Sets the LocalDescription, and starts our UDP listeners
	answer, err := IO.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}
	// IO.LocalSDP, err = answer.Unmarshal()
	// if err != nil {
	// 	return "", err
	// }
	gatherComplete := GatheringCompletePromise(IO.PeerConnection)
	if err = IO.SetLocalDescription(answer); err != nil {
		return nil, err
	}
	<-gatherComplete
	return IO.LocalDescription(), nil
}

func (IO *Connection) Receive() {
	puber := IO.Publisher
	IO.Depend(puber)
	IO.OnTrack(func(track *TrackRemote, receiver *RTPReceiver) {
		IO.Info("OnTrack", "kind", track.Kind().String(), "payloadType", uint8(track.Codec().PayloadType))
		var n int
		var err error
		if codecP := track.Codec(); track.Kind() == RTPCodecTypeAudio {
			if !puber.PubAudio {
				return
			}
			mem := util.NewScalableMemoryAllocator(1 << 12)
			defer mem.Recycle()
			frame := &mrtp.Audio{}
			frame.RTPCodecParameters = &codecP
			frame.SetAllocator(mem)
			for {
				var packet rtp.Packet
				buf := mem.Malloc(mrtp.MTUSize)
				if n, _, err = track.Read(buf); err == nil {
					mem.FreeRest(&buf, n)
					err = packet.Unmarshal(buf)
				}
				if err != nil {
					return
				}
				if len(packet.Payload) == 0 {
					mem.Free(buf)
					continue
				}
				if len(frame.Packets) == 0 || packet.Timestamp == frame.Packets[0].Timestamp {
					frame.AddRecycleBytes(buf)
					frame.Packets = append(frame.Packets, &packet)
				} else {
					err = puber.WriteAudio(frame)
					frame = &mrtp.Audio{}
					frame.AddRecycleBytes(buf)
					frame.Packets = []*rtp.Packet{&packet}
					frame.RTPCodecParameters = &codecP
					frame.SetAllocator(mem)
				}
			}
		} else {
			if !puber.PubVideo {
				return
			}
			var lastPLISent time.Time
			mem := util.NewScalableMemoryAllocator(1 << 12)
			defer mem.Recycle()
			frame := &mrtp.Video{}
			frame.RTPCodecParameters = &codecP
			frame.SetAllocator(mem)
			for {
				if time.Since(lastPLISent) > IO.PLI {
					if rtcpErr := IO.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); rtcpErr != nil {
						puber.Error("writeRTCP", "error", rtcpErr)
						return
					}
					lastPLISent = time.Now()
				}
				var packet rtp.Packet
				buf := mem.Malloc(mrtp.MTUSize)
				if n, _, err = track.Read(buf); err == nil {
					mem.FreeRest(&buf, n)
					err = packet.Unmarshal(buf)
				}
				if err != nil {
					return
				}
				if len(packet.Payload) == 0 {
					mem.Free(buf)
					continue
				}
				if len(frame.Packets) == 0 || packet.Timestamp == frame.Packets[0].Timestamp {
					frame.AddRecycleBytes(buf)
					frame.Packets = append(frame.Packets, &packet)
				} else {
					// t := time.Now()
					err = puber.WriteVideo(frame)
					// fmt.Println("write video", time.Since(t))
					frame = &mrtp.Video{}
					frame.AddRecycleBytes(buf)
					frame.Packets = []*rtp.Packet{&packet}
					frame.RTPCodecParameters = &codecP
					frame.SetAllocator(mem)
				}
			}
		}
	})
	IO.OnDataChannel(func(d *DataChannel) {
		IO.Info("OnDataChannel", "label", d.Label())
		d.OnMessage(func(msg DataChannelMessage) {
			IO.SDP = string(msg.Data[1:])
			IO.Debug("dc message", "sdp", IO.SDP)
			if err := IO.SetRemoteDescription(SessionDescription{Type: SDPTypeOffer, SDP: IO.SDP}); err != nil {
				return
			}
			if answer, err := IO.GetAnswer(); err == nil {
				d.SendText(answer.SDP)
			} else {
				return
			}
			switch msg.Data[0] {
			case '0':
				IO.Stop(errors.New("stop by remote"))
			case '1':

			}
		})
	})
}

func (IO *Connection) Send() (err error) {
	suber := IO.Subscriber
	IO.Depend(suber)
	var useDC bool
	var audioTLSRTP, videoTLSRTP *TrackLocalStaticRTP
	var audioSender, videoSender *RTPSender
	vctx, actx := suber.Publisher.GetVideoCodecCtx(), suber.Publisher.GetAudioCodecCtx()
	if IO.EnableDC && vctx != nil && vctx.FourCC() == codec.FourCC_H265 {
		useDC = true
	}
	if IO.EnableDC && actx != nil && actx.FourCC() == codec.FourCC_MP4A {
		useDC = true
	}

	if vctx != nil && !useDC {
		videoCodec := vctx.FourCC()
		var rcc RTPCodecParameters
		if ctx, ok := vctx.(mrtp.IRTPCtx); ok {
			rcc = ctx.GetRTPCodecParameter()
		} else {
			var rtpCtx mrtp.RTPData
			var tmpAVTrack AVTrack
			tmpAVTrack.ICodecCtx, _, err = rtpCtx.ConvertCtx(vctx)
			if err == nil {
				rcc = tmpAVTrack.ICodecCtx.(mrtp.IRTPCtx).GetRTPCodecParameter()
			} else {
				return
			}
		}
		videoTLSRTP, err = NewTrackLocalStaticRTP(rcc.RTPCodecCapability, videoCodec.String(), suber.StreamPath)
		if err != nil {
			return
		}
		videoSender, err = IO.PeerConnection.AddTrack(videoTLSRTP)
		if err != nil {
			return
		}
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if n, _, rtcpErr := videoSender.Read(rtcpBuf); rtcpErr != nil {
					suber.Warn("rtcp read error", "error", rtcpErr)
					return
				} else {
					if p, err := rtcp.Unmarshal(rtcpBuf[:n]); err == nil {
						for _, pp := range p {
							switch pp.(type) {
							case *rtcp.PictureLossIndication:
								// fmt.Println("PictureLossIndication")
							}
						}
					}
				}
			}
		}()
	}
	if actx != nil && !useDC {
		audioCodec := actx.FourCC()
		var rcc RTPCodecParameters
		if ctx, ok := actx.(mrtp.IRTPCtx); ok {
			rcc = ctx.GetRTPCodecParameter()
		} else {
			var rtpCtx mrtp.RTPData
			var tmpAVTrack AVTrack
			tmpAVTrack.ICodecCtx, _, err = rtpCtx.ConvertCtx(actx)
			if err == nil {
				rcc = tmpAVTrack.ICodecCtx.(mrtp.IRTPCtx).GetRTPCodecParameter()
			} else {
				return
			}
		}
		audioTLSRTP, err = NewTrackLocalStaticRTP(rcc.RTPCodecCapability, audioCodec.String(), suber.StreamPath)
		if err != nil {
			return
		}
		audioSender, err = IO.PeerConnection.AddTrack(audioTLSRTP)
		if err != nil {
			return
		}
	}
	var dc *DataChannel
	if useDC {
		dc, err = IO.CreateDataChannel(suber.StreamPath, nil)
		if err != nil {
			return
		}
		dc.OnOpen(func(){
			var live flv.Live
			live.WriteFlvTag = func(buffers net.Buffers) (err error) {
				r := util.NewReadableBuffersFromBytes(buffers...)
				for r.Length > 65535 {
					r.RangeN(65535, func(buf []byte) {
						err = dc.Send(buf)
						if err != nil {
							fmt.Println("dc send error", err)
						}
					})
				}
				r.Range(func(buf []byte) {
					err = dc.Send(buf)
					if err != nil {
						fmt.Println("dc send error", err)
					}
				})
				return
			}
			live.Subscriber = suber
			err = live.Run()
			dc.Close()
		})
	} else {
		if audioSender == nil {
			suber.SubAudio = false
		}
		if videoSender == nil {
			suber.SubVideo = false
		}
		go m7s.PlayBlock(suber, func(frame *mrtp.Audio) (err error) {
			for _, p := range frame.Packets {
				if err = audioTLSRTP.WriteRTP(p); err != nil {
					return
				}
			}
			return
		}, func(frame *mrtp.Video) error {
			for _, p := range frame.Packets {
				if err := videoTLSRTP.WriteRTP(p); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return
}

func (IO *Connection) Dispose() {
	IO.PeerConnection.Close()
}
