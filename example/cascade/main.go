package main

import (
	"context"
	"flag"

	"m7s.live/v5"
	_ "m7s.live/v5/plugin/cascade"
	_ "m7s.live/v5/plugin/debug"
	_ "m7s.live/v5/plugin/hls"
	_ "m7s.live/v5/plugin/rtmp"
	_ "m7s.live/v5/plugin/rtsp"
)

func main() {
	ctx := context.Background()

	// 级联服务器配置文件路径
	serverConf := flag.String("server", "./cascadeserver.yml", "cascade server config file")

	// 三个级联客户端配置文件路径
	client1Conf := flag.String("client1", "./cascadeclient1.yml", "cascade client1 config file")
	client2Conf := flag.String("client2", "./cascadeclient2.yml", "cascade client2 config file")
	client3Conf := flag.String("client3", "./cascadeclient3.yml", "cascade client3 config file")

	flag.Parse()

	// 启动三个客户端实例（使用goroutine异步运行）
	go startInstance(ctx, *client1Conf, "Client 1")
	go startInstance(ctx, *client2Conf, "Client 2")
	go startInstance(ctx, *client3Conf, "Client 3")

	// 最后启动服务器实例（主进程运行）
	startInstance(ctx, *serverConf, "Server")
}

// 启动单个Monibuca实例的辅助函数
func startInstance(ctx context.Context, confPath, instanceName string) {
	if confPath == "" {
		return
	}
	// 创建带实例名称的上下文，便于日志区分
	ctx = context.WithValue(ctx, "instance", instanceName)
	m7s.Run(ctx, confPath)
}
