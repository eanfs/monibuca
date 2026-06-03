// Package plugin_cluster 提供基于 Consul 的成员管理与跨节点流路由能力。
//
// 实现细节见 //go:build cluster 文件;不带 cluster 构建标签时,
// 本包为空,仅保留 import 路径,允许 example/default 等无条件 import。
//
// 设计基线: doc_CN/arch/cluster.md
package plugin_cluster
