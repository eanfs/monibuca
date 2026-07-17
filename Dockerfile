# 多架构运行镜像 - 支持 AMD64 和 ARM64（使用清华大学镜像源）
# 注意: 不要 pin --platform=$BUILDPLATFORM, 否则 base rootfs 会被钉成构建机架构,
# 导致跨架构镜像 rootfs(sh/curl/glibc) 与目标架构不符。让 base 跟随 TARGETPLATFORM。
FROM swr.cn-east-3.myhuaweicloud.com/intetech/ffmpeg:latest AS base

WORKDIR /monibuca

# 配置清华大学镜像源（DEB822 格式）
# 先清掉 base 镜像里的所有默认源, 避免 apt 同时尝试 archive.ubuntu.com (本地某些网络环境下不可达)
# 注意: 清华 /ubuntu 仅含 amd64/i386; arm64 等 ports 架构在独立的 /ubuntu-ports 仓库,
# 否则 arm64 构建拉 arm64 Packages 会 404 → apt 失败。按 TARGETARCH 选择正确仓库路径。
ARG TARGETARCH
RUN set -eux; \
    if [ "$TARGETARCH" = "amd64" ]; then MP=ubuntu; else MP=ubuntu-ports; fi; \
    rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; \
    printf 'Types: deb\nURIs: https://mirrors.tuna.tsinghua.edu.cn/%s\nSuites: noble noble-updates noble-backports\nComponents: main restricted universe multiverse\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n\nTypes: deb\nURIs: https://mirrors.tuna.tsinghua.edu.cn/%s\nSuites: noble-security\nComponents: main restricted universe multiverse\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n' "$MP" "$MP" > /etc/apt/sources.list.d/ubuntu.sources

# 安装必要的工具（使用清华源加速）
RUN apt-get update && \
    apt-get install -y tcpdump && \
    rm -rf /var/lib/apt/lists/*

# 复制二进制文件 (TARGETARCH 已在上方声明)
COPY monibuca_${TARGETARCH} ./monibuca_linux

# 复制静态资源
COPY example/default/admin.zip ./admin.zip
COPY example/default/test.mp4 ./test.mp4
COPY example/default/test.flv ./test.flv

# 复制配置文件
COPY example/default/config.yaml /etc/monibuca/config.yaml

# 导出端口
EXPOSE 6000 8080 8443 1935 554 5060 9000-20000/udp
EXPOSE 5060/udp 44944/udp

# 设置入口点
ENTRYPOINT ["./monibuca_linux"]
CMD ["-c", "/etc/monibuca/config.yaml"]
