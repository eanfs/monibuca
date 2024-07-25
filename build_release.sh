#!/bin/bash

# 编译Linux AMD64架构的二进制文件
echo "Building for Linux AMD64..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o monibuca-linux-amd64
# 编译Linux ARM64架构的二进制文件
echo "Building for Linux ARM64..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o monibuca-linux-arm64

bash -x build_image.sh 4.7.5


rm -rf monibuca-linux-amd64
rm -rf monibuca-linux-arm64