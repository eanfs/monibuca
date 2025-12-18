package plugin_sei

import (
	"m7s.live/v5"
	"m7s.live/v5/plugin/sei/pb"
	sei "m7s.live/v5/plugin/sei/pkg"
)

var _ = m7s.InstallPlugin[SEIPlugin](m7s.PluginMeta{
	NewTransformer:      sei.NewTransform,
	RegisterGRPCHandler: pb.RegisterApiHandler,
	ServiceDesc:         &pb.Api_ServiceDesc,
})

type SEIPlugin struct {
	pb.UnimplementedApiServer
	m7s.Plugin
}
