package plugin_debug

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/process"
	"m7s.live/v5/pkg/task"
)

//go:embed static/*
var staticFS embed.FS
var staticFSHandler = http.FileServer(http.FS(staticFS))

type update struct {
	Ts             int64
	BytesAllocated uint64
	GcPause        uint64
	CPUUser        float64
	CPUSys         float64
	Block          int
	Goroutine      int
	Heap           int
	Mutex          int
	Threadcreate   int
}

type consumer struct {
	id uint
	c  chan update
}

type server struct {
	task.TickTask
	consumers      []consumer
	consumersMutex sync.RWMutex
	data           DataStorage
	lastPause      uint32
	dataMutex      sync.RWMutex
	lastConsumerID uint
	upgrader       websocket.Upgrader
	prevSysTime    float64
	prevUserTime   float64
	myProcess      *process.Process
}

type SimplePair struct {
	Ts    uint64
	Value uint64
}

type CPUPair struct {
	Ts   uint64
	User float64
	Sys  float64
}

type PprofPair struct {
	Ts           uint64
	Block        int
	Goroutine    int
	Heap         int
	Mutex        int
	Threadcreate int
}

type DataStorage struct {
	BytesAllocated []SimplePair
	GcPauses       []SimplePair
	CPUUsage       []CPUPair
	Pprof          []PprofPair
}

const (
	maxCount int = 86400
)

func (s *server) Start() error {
	var err error
	s.myProcess, err = process.NewProcess(int32(os.Getpid()))
	if err != nil {
		log.Printf("Failed to get process: %v", err)
	}
	// 初始化 WebSocket upgrader
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	// preallocate arrays in data, helps save on reallocations caused by append()
	// when maxCount is large
	s.data.BytesAllocated = make([]SimplePair, 0, maxCount)
	s.data.GcPauses = make([]SimplePair, 0, maxCount)
	s.data.CPUUsage = make([]CPUPair, 0, maxCount)
	s.data.Pprof = make([]PprofPair, 0, maxCount)
	return s.TickTask.Start()
}

func (s *server) GetTickInterval() time.Duration {
	return time.Second
}

func (s *server) Tick(any) {
	now := time.Now()
	nowUnix := now.Unix()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	u := update{
		Ts:           nowUnix * 1000,
		Block:        pprof.Lookup("block").Count(),
		Goroutine:    pprof.Lookup("goroutine").Count(),
		Heap:         pprof.Lookup("heap").Count(),
		Mutex:        pprof.Lookup("mutex").Count(),
		Threadcreate: pprof.Lookup("threadcreate").Count(),
	}
	s.data.Pprof = append(s.data.Pprof, PprofPair{
		uint64(nowUnix) * 1000,
		u.Block,
		u.Goroutine,
		u.Heap,
		u.Mutex,
		u.Threadcreate,
	})

	cpuTimes, err := s.myProcess.Times()
	if err != nil {
		cpuTimes = &cpu.TimesStat{}
	}

	if s.prevUserTime != 0 {
		u.CPUUser = cpuTimes.User - s.prevUserTime
		u.CPUSys = cpuTimes.System - s.prevSysTime
		s.data.CPUUsage = append(s.data.CPUUsage, CPUPair{uint64(nowUnix) * 1000, u.CPUUser, u.CPUSys})
	}

	s.prevUserTime = cpuTimes.User
	s.prevSysTime = cpuTimes.System

	s.dataMutex.Lock()

	bytesAllocated := ms.Alloc
	u.BytesAllocated = bytesAllocated
	s.data.BytesAllocated = append(s.data.BytesAllocated, SimplePair{uint64(nowUnix) * 1000, bytesAllocated})
	if s.lastPause == 0 || s.lastPause != ms.NumGC {
		gcPause := ms.PauseNs[(ms.NumGC+255)%256]
		u.GcPause = gcPause
		s.data.GcPauses = append(s.data.GcPauses, SimplePair{uint64(nowUnix) * 1000, gcPause})
		s.lastPause = ms.NumGC
	}

	if len(s.data.BytesAllocated) > maxCount {
		s.data.BytesAllocated = s.data.BytesAllocated[len(s.data.BytesAllocated)-maxCount:]
	}

	if len(s.data.GcPauses) > maxCount {
		s.data.GcPauses = s.data.GcPauses[len(s.data.GcPauses)-maxCount:]
	}

	s.dataMutex.Unlock()

	s.sendToConsumers(u)
}

func (s *server) sendToConsumers(u update) {
	s.consumersMutex.RLock()
	defer s.consumersMutex.RUnlock()

	for _, c := range s.consumers {
		c.c <- u
	}
}

func (s *server) removeConsumer(id uint) {
	s.consumersMutex.Lock()
	defer s.consumersMutex.Unlock()

	var consumerID uint
	var consumerFound bool

	for i, c := range s.consumers {
		if c.id == id {
			consumerFound = true
			consumerID = uint(i)
			break
		}
	}

	if consumerFound {
		s.consumers = append(s.consumers[:consumerID], s.consumers[consumerID+1:]...)
	}
}

func (s *server) addConsumer() consumer {
	s.consumersMutex.Lock()
	defer s.consumersMutex.Unlock()

	s.lastConsumerID++

	c := consumer{
		id: s.lastConsumerID,
		c:  make(chan update),
	}

	s.consumers = append(s.consumers, c)

	return c
}

func (s *server) dataFeedHandler(w http.ResponseWriter, r *http.Request) {
	var (
		lastPing time.Time
		lastPong time.Time
	)

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	conn.SetPongHandler(func(s string) error {
		lastPong = time.Now()
		return nil
	})

	// read and discard all messages
	go func(c *websocket.Conn) {
		for {
			if _, _, err := c.NextReader(); err != nil {
				c.Close()
				break
			}
		}
	}(conn)

	c := s.addConsumer()

	defer func() {
		s.removeConsumer(c.id)
		conn.Close()
	}()

	var i uint

	for u := range c.c {
		conn.WriteJSON(u)
		i++

		if i%10 == 0 {
			if diff := lastPing.Sub(lastPong); diff > time.Second*60 {
				return
			}
			now := time.Now()
			if err := conn.WriteControl(websocket.PingMessage, nil, now.Add(time.Second)); err != nil {
				return
			}
			lastPing = now
		}
	}
}

func (s *server) dataHandler(w http.ResponseWriter, r *http.Request) {
	s.dataMutex.RLock()
	defer s.dataMutex.RUnlock()

	if e := r.ParseForm(); e != nil {
		log.Print("error parsing form")
		return
	}

	callback := r.FormValue("callback")

	fmt.Fprintf(w, "%v(", callback)

	w.Header().Set("Content-Type", "application/json")

	encoder := json.NewEncoder(w)
	encoder.Encode(s.data)

	fmt.Fprint(w, ")")
}
