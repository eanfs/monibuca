package hls

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/quangngotan95/go-m3u8/m3u8"
)

var MemoryM3u8 sync.Map
var MemoryTs sync.Map

type TsCacher interface {
	GetTs(key string) (any, bool)
}
type M3u8Info struct {
	Req       *http.Request
	M3U8Count int    //一共拉取的m3u8文件数量
	TSCount   int    //一共拉取的ts文件数量
	LastM3u8  string //最后一个m3u8文件内容
}

type TSDownloader struct {
	client *http.Client
	url    *url.URL
	req    *http.Request
	res    *http.Response
	wg     sync.WaitGroup
	err    error
	dur    float64
}

func (p *TSDownloader) Start() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if tsRes, err := p.client.Do(p.req); err == nil {
			p.res = tsRes
		} else {
			p.err = err
		}
	}()
}

func readM3U8(res *http.Response) (playlist *m3u8.Playlist, err error) {
	var reader io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		reader, err = gzip.NewReader(reader)
	}
	if err == nil {
		playlist, err = m3u8.Read(reader)
	}
	if err != nil {
		// HLSPlugin.Error("readM3U8", zap.Error(err))
	}
	return
}

const (
	HLS_KEY_METHOD_AES_128 = "AES-128"
)

// https://datatracker.ietf.org/doc/draft-pantos-http-live-streaming/

// 以”#EXT“开头的表示一个”tag“,否则表示注释,直接忽略
type Playlist struct {
	io.Writer
	ExtM3U         string      // indicates that the file is an Extended M3U [M3U] Playlist file. (4.3.3.1) -- 每个M3U文件第一行必须是这个tag.
	Version        int         // indicates the compatibility version of the Playlist file. (4.3.1.2) -- 协议版本号.
	Sequence       int         // indicates the Media Sequence Number of the first Media Segment that appears in a Playlist file. (4.3.3.2) -- 第一个媒体段的序列号.
	Targetduration int         // specifies the maximum Media Segment duration. (4.3.3.1) -- 每个视频分段最大的时长(单位秒).
	PlaylistType   int         // rovides mutability information about the Media Playlist file. (4.3.3.5) -- 提供关于PlayList的可变性的信息.
	Discontinuity  int         // indicates a discontinuity between theMedia Segment that follows it and the one that preceded it. (4.3.2.3) -- 该标签后边的媒体文件和之前的媒体文件之间的编码不连贯(即发生改变)(场景用于插播广告等等).
	Key            PlaylistKey // specifies how to decrypt them. (4.3.2.4) -- 解密媒体文件的必要信息(表示怎么对media segments进行解码).
	EndList        string      // indicates that no more Media Segments will be added to the Media Playlist file. (4.3.3.4) -- 标示没有更多媒体文件将会加入到播放列表中,它可能会出现在播放列表文件的任何地方,但是不能出现两次或以上.
	Inf            PlaylistInf // specifies the duration of a Media Segment. (4.3.2.1) -- 指定每个媒体段(ts)的持续时间.
	tsCount        int
}

// Discontinuity :
// file format
// number, type and identifiers of tracks
// timestamp sequence
// encoding parameters
// encoding sequence

type PlaylistKey struct {
	Method string // specifies the encryption method. (4.3.2.4)
	Uri    string // key url. (4.3.2.4)
	IV     string // key iv. (4.3.2.4)
}

type PlaylistInf struct {
	Duration float64
	Title    string
	FilePath string
}

func (pl *Playlist) Init() (err error) {
	// ss := fmt.Sprintf("#EXTM3U\n"+
	// 	"#EXT-X-VERSION:%d\n"+
	// 	"#EXT-X-MEDIA-SEQUENCE:%d\n"+
	// 	"#EXT-X-TARGETDURATION:%d\n"+
	// 	"#EXT-X-PLAYLIST-TYPE:%d\n"+
	// 	"#EXT-X-DISCONTINUITY:%d\n"+
	// 	"#EXT-X-KEY:METHOD=%s,URI=%s,IV=%s\n"+
	// 	"#EXT-X-ENDLIST", hls.Version, hls.Sequence, hls.Targetduration, hls.PlaylistType, hls.Discontinuity, hls.Key.Method, hls.Key.Uri, hls.Key.IV)
	_, err = fmt.Fprintf(pl, "#EXTM3U\n"+
		"#EXT-X-VERSION:%d\n"+
		"#EXT-X-MEDIA-SEQUENCE:%d\n"+
		"#EXT-X-TARGETDURATION:%d\n", pl.Version, pl.Sequence, pl.Targetduration)
	pl.Sequence++
	return
}

func (pl *Playlist) WriteInf(inf PlaylistInf) (err error) {
	_, err = fmt.Fprintf(pl, "#EXTINF:%.3f,\n"+
		"%s\n", inf.Duration, inf.Title)
	pl.tsCount++
	return
}
