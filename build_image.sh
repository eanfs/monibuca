#!/bin/bash

# 创建Docker镜像并打包
echo "Creating Docker images..."


artifactId=monibuca
version=${1}

group=swr.cn-east-3.myhuaweicloud.com/intetech

# docker
# docker buildx build --platform linux/amd64,linux/arm64 -t ${artifactId}:${version} --push -f ./Dockerfile .
docker buildx build --platform=linux/amd64 --no-cache --progress=plain -t ${artifactId}:${version}-amd64 -f ./Dockerfile.amd64 -o type=docker,dest=- . > ${artifactId}-amd64.tar
docker buildx build --platform=linux/arm64 --no-cache --progress=plain -t ${artifactId}:${version}-arm64 -f ./Dockerfile.arm64 -o type=docker,dest=- . > ${artifactId}-arm64.tar
# #docker tag  ${artifactId}:${version}  ${artifactId}:latest

docker load < ${artifactId}-amd64.tar
docker load < ${artifactId}-arm64.tar

# docker buildx imagetools inspect lrc32/xde-temurin:11-jdk-focal-cn

docker tag ${artifactId}:${version}-amd64 ${group}/${artifactId}:${version}-amd64
docker push ${group}/${artifactId}:${version}-amd64
docker rmi ${artifactId}:${version}-amd64
docker rmi ${group}/${artifactId}:${version}-amd64

docker tag ${artifactId}:${version}-arm64 ${group}/${artifactId}:${version}-arm64
docker push ${group}/${artifactId}:${version}-arm64
docker rmi ${artifactId}:${version}-arm64
docker rmi ${group}/${artifactId}:${version}-arm64

# push version manifest
docker manifest rm ${group}/${artifactId}:${version}
docker manifest create  --amend ${group}/${artifactId}:${version} \
${group}/${artifactId}:${version}-amd64 \
${group}/${artifactId}:${version}-arm64

docker manifest push ${group}/${artifactId}:${version}

# push latest manifest
docker manifest rm ${group}/${artifactId}:latest
docker manifest create --amend ${group}/${artifactId}:latest \
 ${group}/${artifactId}:${version}-amd64 \
 ${group}/${artifactId}:${version}-arm64
docker manifest push ${group}/${artifactId}:latest


rm -rf ${artifactId}-amd64.tar
rm -rf ${artifactId}-arm64.tar


echo "Build and Docker image creation complete."