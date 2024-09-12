package plugin_mp4

import (
	"net/http"
	"strings"
	"time"

	"m7s.live/m7s/v5"
	"m7s.live/m7s/v5/pkg/util"
)

func (p *MP4Plugin) download(w http.ResponseWriter, r *http.Request) {
	filePath := r.PathValue("filePath")
	query := r.URL.Query()
	rangeStr := strings.Split(query.Get("range"), "~")
	var startTime, endTime time.Time
	if len(rangeStr) != 2 {
		http.NotFound(w, r)
		return
	}
	var err error
	startTime, err = util.TimeQueryParse(rangeStr[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	endTime, err = util.TimeQueryParse(rangeStr[1])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	//timeRange := endTime.Sub(startTime)
	p.Info("download", "filePath", filePath, "start", startTime, "end", endTime)
	var streams []m7s.RecordStream
	p.DB.Find(&streams, "end_time>? AND start_time<? AND file_path=?", startTime, endTime, filePath)
	// muxer, err := mp4.NewMuxer(w, 0)
	// for i, stream := range streams {
	// 	file, err := os.Open(filepath.Join(filePath, fmt.Sprintf("%d.mp4", stream.ID)))
	// 	if err != nil {
	// 		return
	// 	}
	// 	demuxer := mp4.NewDemuxer(file)
	// 	var tracks []box.TrackInfo
	// 	if tracks, err = demuxer.ReadHead(); err != nil {
	// 		return
	// 	}
	// }
}
