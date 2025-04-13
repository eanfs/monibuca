package plugin_transcode

import (
	m7s "m7s.live/v5"
	"m7s.live/v5/plugin/transcode/pb"
	transcode "m7s.live/v5/plugin/transcode/pkg"
)

var _ = m7s.InstallPlugin[TranscodePlugin](m7s.PluginMeta{
	NewTransformer:      transcode.NewTransform,
	RegisterGRPCHandler: pb.RegisterApiHandler,
	ServiceDesc:         &pb.Api_ServiceDesc,
})

type TranscodePlugin struct {
	pb.UnimplementedApiServer
	m7s.Plugin
	LogToFile string
}
