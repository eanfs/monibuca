package main

import (
	"context"
	"flag"
	"log"

	"m7s.live/v5"
	_ "m7s.live/v5/plugin/cluster" // 集群管理
	_ "m7s.live/v5/plugin/flv"     // FLV 插件
)

func main() {
	conf := flag.String("c", "etcd-node1.yaml", "config file")
	flag.Parse()

	log.Printf("Cluster 测试程序启动, 配置文件: %s", *conf)

	// 使用最简单的方式启动服务器
	m7s.Run(context.Background(), *conf)
}
