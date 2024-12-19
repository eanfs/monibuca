package rtsp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"gopkg.in/yaml.v3"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/util"
	mrtp "m7s.live/v5/plugin/rtp/pkg"
)

func parseRTSPDump(filename string) ([]Packet, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var dump struct {
		Packets []struct {
			Packet    int     `yaml:"packet"`
			Peer      int     `yaml:"peer"`
			Index     int     `yaml:"index"`
			Timestamp float64 `yaml:"timestamp"`
			Data      string  `yaml:"data"`
		} `yaml:"packets"`
	}

	err = yaml.Unmarshal(data, &dump)
	if err != nil {
		return nil, err
	}

	packets := make([]Packet, 0, len(dump.Packets))
	for _, p := range dump.Packets {
		packets = append(packets, Packet{
			Index:     p.Index,
			Peer:      p.Peer,
			Timestamp: p.Timestamp,
			Data:      []byte(p.Data),
		})
	}

	return packets, nil
}

type RTSPMockConn struct {
	packets      []Packet
	currentIndex int
	peer         int
	readDeadline time.Time
	closed       bool
	localAddr    net.Addr
	remoteAddr   net.Addr
}

type Packet struct {
	Index     int
	Timestamp float64
	Peer      int
	Data      []byte
}

func NewRTSPMockConn(dumpFile string, peer int) (*RTSPMockConn, error) {
	// Parse YAML dump file and extract packets
	packets, err := parseRTSPDump(dumpFile)
	if err != nil {
		return nil, err
	}

	return &RTSPMockConn{
		packets:      packets,
		currentIndex: 0,
		peer:         peer,
		localAddr:    &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8554},
		remoteAddr:   &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 49152},
	}, nil
}

// Read implements net.Conn interface
func (c *RTSPMockConn) Read(b []byte) (n int, err error) {
	if c.closed {
		return 0, io.EOF
	}

	if c.currentIndex >= len(c.packets) {
		return 0, io.EOF
	}

	// Check read deadline
	if !c.readDeadline.IsZero() && time.Now().After(c.readDeadline) {
		return 0, os.ErrDeadlineExceeded
	}
	packet := c.packets[c.currentIndex]
	for packet.Peer != c.peer {
		c.currentIndex++
		packet = c.packets[c.currentIndex]
	}

	n = copy(b, packet.Data)
	if n == len(packet.Data) {
		c.currentIndex++
	} else {
		packet.Data = packet.Data[n:]
	}

	return n, nil
}

// Write implements net.Conn interface - just discard data
func (c *RTSPMockConn) Write(b []byte) (n int, err error) {
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

// Close implements net.Conn interface
func (c *RTSPMockConn) Close() error {
	c.closed = true
	return nil
}

// LocalAddr implements net.Conn interface
func (c *RTSPMockConn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr implements net.Conn interface
func (c *RTSPMockConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline implements net.Conn interface
func (c *RTSPMockConn) SetDeadline(t time.Time) error {
	c.readDeadline = t
	return nil
}

// SetReadDeadline implements net.Conn interface
func (c *RTSPMockConn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	return nil
}

// SetWriteDeadline implements net.Conn interface
func (c *RTSPMockConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func TestNetConnection_Receive(t *testing.T) {
	conn, err := NewRTSPMockConn("/Users/dexter/project/v5/monibuca/example/default/dump/rtsp",0)
	if err != nil {
		t.Fatal(err)
	}
	allocator := util.NewScalableMemoryAllocator(1 << 12)
	audioFrame, videoFrame := &mrtp.Audio{}, &mrtp.Video{}
	audioFrame.RTPCodecParameters = &webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: "audio/MPEG4-GENERIC",
		},
	}
	audioFrame.SetAllocator(allocator)
	videoFrame.RTPCodecParameters = &webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264,
		},
	}
	videoFrame.SetAllocator(allocator)
	c := NewNetConnection(conn)
	c.Logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	c.Context, c.CancelCauseFunc = context.WithCancelCause(context.Background())
	var videoTrack *pkg.AVTrack
	videoTrack = pkg.NewAVTrack(&mrtp.Video{}, c.Logger.With("track", "video"), &config.Publish{
		RingSize: util.Range[int]{20, 1024},
	}, util.NewPromise(context.Background()))
	videoTrack.ICodecCtx = &mrtp.H264Ctx{}
	if err := c.Receive(false, func(channelID byte, buf []byte) error {
		switch int(channelID) {
		case 2:
			packet := &rtp.Packet{}
			if err = packet.Unmarshal(buf); err != nil {
				return err
			}
			if len(audioFrame.Packets) == 0 || packet.Timestamp == audioFrame.Packets[0].Timestamp {
				audioFrame.AddRecycleBytes(buf)
				audioFrame.Packets = append(audioFrame.Packets, packet)
				return nil
			} else {
				// if err = r.WriteAudio(audioFrame); err != nil {
				// 	return err
				// }
				audioFrame = &mrtp.Audio{}
				audioFrame.AddRecycleBytes(buf)
				audioFrame.Packets = []*rtp.Packet{packet}
				// audioFrame.RTPCodecParameters = c.AudioCodecParameters
				audioFrame.SetAllocator(allocator)
				return nil
			}
		case 0:
			packet := &rtp.Packet{}
			if err = packet.Unmarshal(buf); err != nil {
				return err
			}
			if len(videoFrame.Packets) == 0 || packet.Timestamp == videoFrame.Packets[0].Timestamp {
				videoFrame.AddRecycleBytes(buf)
				videoFrame.Packets = append(videoFrame.Packets, packet)
				return nil
			} else {
				videoFrame.Parse(videoTrack)
				// t := time.Now()
				// if err = r.WriteVideo(videoFrame); err != nil {
				// 	return err
				// }
				fmt.Println("write video", videoTrack.Value.Raw)
				videoFrame = &mrtp.Video{}
				videoFrame.RTPCodecParameters = &webrtc.RTPCodecParameters{
					RTPCodecCapability: webrtc.RTPCodecCapability{
						MimeType: webrtc.MimeTypeH264,
					},
				}
				videoFrame.AddRecycleBytes(buf)
				videoFrame.Packets = []*rtp.Packet{packet}
				// videoFrame.RTPCodecParameters = c.VideoCodecParameters
				videoFrame.SetAllocator(allocator)
				return nil
			}
		default:

		}
		return pkg.ErrUnsupportCodec
	}, func(channelID byte, buf []byte) error {
		msg := &RTCP{Channel: channelID}
		if err = msg.Header.Unmarshal(buf); err != nil {
			return err
		}
		if msg.Packets, err = rtcp.Unmarshal(buf); err != nil {
			return err
		}
		// r.Stream.Debug("rtcp", "type", msg.Header.Type, "length", msg.Header.Length)
		// TODO: rtcp msg
		return pkg.ErrDiscard
	}); err != nil {
		t.Errorf("NetConnection.Receive() error = %v", err)
	}
}

func TestNetConnection_Pull(t *testing.T) {
	conn, err := NewRTSPMockConn("/Users/dexter/project/v5/monibuca/example/default/dump/rtsp", 1)
	if err != nil {
		t.Fatal(err)
	}
	client := NewPuller(config.Pull{
		URL: "rtsp://127.0.0.1:8554/dump/test",
	}).(*Client)
	client.NetConnection = &NetConnection{Conn: conn}
	client.BufReader = util.NewBufReader(conn)
	client.URL, _ = url.Parse("rtsp://127.0.0.1:8554/dump/test")
	client.MemoryAllocator = util.NewScalableMemoryAllocator(1 << 12)
	client.Logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	client.Context, client.CancelCauseFunc = context.WithCancelCause(context.Background())
	client.Run()
}
