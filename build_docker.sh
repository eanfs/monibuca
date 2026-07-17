#!/bin/bash

# 构建多架构 Docker 镜像并推送到私有仓库
set -e

echo "=========================================="
echo "开始构建多架构 Docker 镜像"
echo "=========================================="

# 获取版本信息
artifactId=monibuca
if [ -z "$1" ]; then
  # 默认获取最新 git tag
  version=$(git describe --tags --abbrev=0 2>/dev/null || echo "")
  if [ -z "$version" ]; then
    echo "错误: 无法获取 git tag，请手动指定版本号"
    echo "用法: $0 <version>"
    exit 1
  fi
  echo "使用最新 git tag: $version"
else
  version=$1
  echo "使用指定版本: $version"
fi

group=swr.cn-east-3.myhuaweicloud.com/intetech

# 切换到 example/default 目录进行编译
cd example/cluster || exit 1

# 清理旧的编译产物
echo "清理旧的编译产物..."
rm -f ../../monibuca_amd64 ../../monibuca_arm64 2>/dev/null || true

# 编译 Linux AMD64 架构的二进制文件
echo ""
echo "编译 Linux AMD64 架构..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags cluster,postgres,sqlite,s3 -o ../../monibuca_amd64 .
echo "✓ AMD64 编译完成"

# 编译 Linux ARM64 架构的二进制文件
echo ""
echo "编译 Linux ARM64 架构..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags cluster,postgres,sqlite,s3 -o ../../monibuca_arm64 .
echo "✓ ARM64 编译完成"

# 返回根目录
cd ../..

# 构建 AMD64 Docker 镜像并加载到本地
echo ""
echo "=========================================="
echo "构建 AMD64 Docker 镜像..."
echo "=========================================="
# SWR 不认 buildkit attestation manifest("Invalid image, fail to parse manifest.json"),
# 显式关闭 provenance/sbom,不依赖 --load 恰好剥离它们的巧合行为。
docker buildx build \
  --platform=linux/amd64 \
  --no-cache \
  --progress=plain \
  --provenance=false \
  --sbom=false \
  --load \
  -t ${artifactId}:${version}-amd64 \
  -f ./Dockerfile .

# 构建 ARM64 Docker 镜像并加载到本地
echo ""
echo "=========================================="
echo "构建 ARM64 Docker 镜像..."
echo "=========================================="
docker buildx build \
  --platform=linux/arm64 \
  --no-cache \
  --progress=plain \
  --provenance=false \
  --sbom=false \
  --load \
  -t ${artifactId}:${version}-arm64 \
  -f ./Dockerfile .


# 推送到私有仓库
echo ""
echo "=========================================="
echo "推送到私有仓库..."
echo "=========================================="

# 推送 AMD64
echo "推送 AMD64 镜像..."
docker tag ${artifactId}:${version}-amd64 ${group}/${artifactId}:${version}-amd64
docker push ${group}/${artifactId}:${version}-amd64

# 删除本地标签
docker rmi ${artifactId}:${version}-amd64
docker rmi ${group}/${artifactId}:${version}-amd64

# 推送 ARM64
echo "推送 ARM64 镜像..."
docker tag ${artifactId}:${version}-arm64 ${group}/${artifactId}:${version}-arm64
docker push ${group}/${artifactId}:${version}-arm64

# 删除本地标签
docker rmi ${artifactId}:${version}-arm64
docker rmi ${group}/${artifactId}:${version}-arm64

# 创建并推送多架构 manifest
echo ""
echo "=========================================="
echo "创建多架构 manifest..."
echo "=========================================="

# 删除已存在的 manifest
docker manifest rm ${group}/${artifactId}:${version} 2>/dev/null || true

# 创建版本 manifest
echo "创建版本 manifest..."
docker manifest create \
  --amend ${group}/${artifactId}:${version} \
  ${group}/${artifactId}:${version}-amd64 \
  ${group}/${artifactId}:${version}-arm64

# 推送版本 manifest
docker manifest push ${group}/${artifactId}:${version}

# 创建并推送 latest manifest
echo "创建并推送 latest manifest..."
docker manifest rm ${group}/${artifactId}:latest 2>/dev/null || true
docker manifest create \
  --amend ${group}/${artifactId}:latest \
  ${group}/${artifactId}:${version}-amd64 \
  ${group}/${artifactId}:${version}-arm64
docker manifest push ${group}/${artifactId}:latest

# 清理临时文件
echo ""
echo "清理临时文件..."

rm -f monibuca_amd64 monibuca_arm64 2>/dev/null || true

echo ""
echo "=========================================="
echo "✓ 多架构镜像构建并推送完成"
echo "镜像地址: ${group}/${artifactId}:${version}"
echo "支持架构: linux/amd64, linux/arm64"
echo "=========================================="
