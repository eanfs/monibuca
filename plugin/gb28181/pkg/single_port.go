package gb28181

import (
	"context"
	"fmt"
	"io"
	"net"

	task "github.com/langhuihui/gotask"
	"github.com/pion/rtp"
	"m7s.live/v5/pkg/util"
)

type SinglePortReader struct {
	SSRC     uint32
	conn     io.ReadCloser
	buffered util.Buffer
	Mouth    chan []byte
	Context  context.Context
}

func (s *SinglePortReader) GetKey() uint32 {
	return s.SSRC
}

func (s *SinglePortReader) Read(buf []byte) (n int, err error) {
	if s.buffered.Len() > 0 {
		return s.buffered.Read(buf)
	}
	if s.conn != nil {
		return s.conn.Read(buf)
	}
	// 添加对 Context 的检查，如果上下文已取消则返回 EOF
	select {
	case s.buffered = <-s.Mouth:
		return s.Read(buf)
	case <-s.Context.Done():
		return 0, s.Context.Err()
	}
}

func (s *SinglePortReader) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

type SinglePortUDP struct {
	task.Task
	Port uint16
	conn *net.UDPConn
	*util.Collection[uint32, *SinglePortReader]
}

type SinglePortTCP struct {
	task.Task
	Port uint16
	net.Listener
	*util.Collection[uint32, *SinglePortReader]
}

func (s *SinglePortUDP) Start() (err error) {
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", s.Port))
	if err != nil {
		return err
	}
	s.conn, err = net.ListenUDP("udp4", addr)
	if err == nil {
		s.OnStop(func() {
			s.conn.Close()
		})
	}
	return
}

func (s *SinglePortUDP) Go() (err error) {
	buffer := make([]byte, 2048) // 足够大的缓冲区来接收UDP包
	for {
		n, _, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			return err
		}

		var packet rtp.Packet
		err = packet.Unmarshal(buffer[:n])
		if err != nil {
			continue // 忽略无法解析的包
		}

		r, _ := s.LoadOrStore(&SinglePortReader{
			SSRC:  packet.SSRC,
			Mouth: make(chan []byte, 100),
		})

		// 创建一个新的缓冲区，包含当前接收到的数据
		packetBytes := make([]byte, n)
		copy(packetBytes, buffer[:n])
		select {
		case r.Mouth <- packetBytes:
		default:
			// 如果通道已满，则忽略该包
		}
	}
}

func (s *SinglePortTCP) Start() (err error) {
	s.Listener, err = net.Listen("tcp4", fmt.Sprintf(":%d", s.Port))
	if err == nil {
		s.OnStop(s.Listener.Close)
	}
	return
}

func (s *SinglePortTCP) Go() (err error) {
	for {
		var packet rtp.Packet
		var lenBytes [2]byte
		conn, err := s.Listener.Accept()
		if err != nil {
			return err
		}
		_, err = io.ReadFull(conn, lenBytes[:])
		if err != nil {
			return err
		}
		packetLength := int(lenBytes[0])<<8 | int(lenBytes[1])
		packetBytes := make([]byte, packetLength+2)
		packetBytes[0] = lenBytes[0]
		packetBytes[1] = lenBytes[1]
		_, err = io.ReadFull(conn, packetBytes[2:])
		if err != nil {
			return err
		}
		err = packet.Unmarshal(packetBytes[2:])
		if err != nil {
			return err
		}
		r, _ := s.LoadOrStore(&SinglePortReader{
			SSRC:  packet.SSRC,
			Mouth: make(chan []byte, 10),
		})
		r.buffered = packetBytes
		r.conn = conn
		r.Mouth <- packetBytes
	}
}
