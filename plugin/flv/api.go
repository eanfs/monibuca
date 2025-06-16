package plugin_flv

import (
	"context"
	"net/http"

	"google.golang.org/protobuf/types/known/emptypb"
	"m7s.live/v5/pb"
	flvpb "m7s.live/v5/plugin/flv/pb"
)

func (p *FLVPlugin) List(ctx context.Context, req *flvpb.ReqRecordList) (resp *pb.RecordResponseList, err error) {
	globalReq := &pb.ReqRecordList{
		StreamPath: req.StreamPath,
		Range:      req.Range,
		Start:      req.Start,
		End:        req.End,
		PageNum:    req.PageNum,
		PageSize:   req.PageSize,
		Type:       "flv",
	}
	return p.Server.GetRecordList(ctx, globalReq)
}

func (p *FLVPlugin) Catalog(ctx context.Context, req *emptypb.Empty) (resp *pb.ResponseCatalog, err error) {
	return p.Server.GetRecordCatalog(ctx, &pb.ReqRecordCatalog{Type: "flv"})
}

func (p *FLVPlugin) Delete(ctx context.Context, req *flvpb.ReqRecordDelete) (resp *pb.ResponseDelete, err error) {
	globalReq := &pb.ReqRecordDelete{
		StreamPath: req.StreamPath,
		Ids:        req.Ids,
		StartTime:  req.StartTime,
		EndTime:    req.EndTime,
		Range:      req.Range,
		Type:       "flv",
	}
	return p.Server.DeleteRecord(ctx, globalReq)
}

func (plugin *FLVPlugin) Download_(w http.ResponseWriter, r *http.Request) {
	// 解析请求参数
	params, err := plugin.parseRequestParams(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	plugin.Info("download", "stream", params.streamPath, "start", params.startTime, "end", params.endTime)

	// 从数据库查询录像记录
	recordStreams, err := plugin.queryRecordStreams(params)
	if err != nil {
		plugin.Error("Failed to query record streams", "err", err)
		http.Error(w, "Database query failed", http.StatusInternalServerError)
		return
	}

	// 构建文件信息列表
	fileInfoList, found := plugin.buildFileInfoList(recordStreams, params.startTime, params.endTime)
	if !found || len(fileInfoList) == 0 {
		plugin.Warn("No records found", "stream", params.streamPath, "start", params.startTime, "end", params.endTime)
		http.NotFound(w, r)
		return
	}

	// 根据记录类型选择处理方式
	if plugin.hasOnlyMp4Records(fileInfoList) {
		// 过滤MP4文件并转换为FLV
		mp4FileList := plugin.filterMp4Files(fileInfoList)
		if len(mp4FileList) == 0 {
			plugin.Warn("No valid MP4 files after filtering", "stream", params.streamPath)
			http.NotFound(w, r)
			return
		}
		plugin.processMp4ToFlv(w, r, mp4FileList, params)
	} else {
		// 过滤FLV文件并处理
		flvFileList := plugin.filterFlvFiles(fileInfoList)
		if len(flvFileList) == 0 {
			plugin.Warn("No valid FLV files after filtering", "stream", params.streamPath)
			http.NotFound(w, r)
			return
		}
		plugin.processFlvFiles(w, r, flvFileList, params)
	}
}
