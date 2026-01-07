FROM alpine:latest

# 安装依赖
RUN apk update && apk add --no-cache \
    ca-certificates wget unzip curl bash jq nodejs 

# Tailscale 最新版本 (2026年1月)
ENV TS_VERSION=1.92.3
ENV TS_ARCH=amd64
RUN wget https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_${TS_ARCH}.tgz && \
    tar xzf tailscale_${TS_VERSION}_${TS_ARCH}.tgz && \
    mv tailscale_${TS_VERSION}_${TS_ARCH}/tailscaled /usr/bin/tailscaled && \
    mv tailscale_${TS_VERSION}_${TS_ARCH}/tailscale /usr/bin/tailscale && \
    rm -rf tailscale_*

# Headscale 最新版本 (0.23.0)
ENV HEADSCALE_VERSION=0.28.0-beta.1
RUN wget https://github.com/juanfont/headscale/releases/download/v${HEADSCALE_VERSION}/headscale_${HEADSCALE_VERSION}_linux_${TS_ARCH} && \
    cp headscale_${HEADSCALE_VERSION}_linux_${TS_ARCH} headscale && \
    mv headscale /usr/bin/headscale && \
    chmod +x /usr/bin/headscale && \
    rm headscale_${HEADSCALE_VERSION}_linux_${TS_ARCH}


# 创建必要的目录
RUN mkdir -p /var/lib/headscale /var/lib/tailscale /var/run/tailscale /etc/headscale

# 安装 GOST
RUN wget https://github.com/go-gost/gost/releases/download/v3.2.6/gost_3.2.6_linux_amd64.tar.gz && \
    tar xzf gost_3.2.6_linux_amd64.tar.gz && \
    mv gost-linux-amd64 /usr/local/bin/gost && \
    chmod +x /usr/local/bin/gost

# 复制配置
COPY gost-config.yaml /etc/gost/config.yaml


# 复制启动脚本
COPY config.yaml /etc/headscale/config.yaml
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# 暴露端口 (Headscale 默认端口)
EXPOSE 8080

# 设置入口
CMD ["/entrypoint.sh"]
