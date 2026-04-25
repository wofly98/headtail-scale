# Headscale Docker 部署与环境配置说明

本项目提供了一个集成了 Headscale 与 Tailscale 的 Docker 部署方案，主要使用 `entrypoint.sh` 在启动容器时完成自动化配置与服务拉起。

## 环境变量配置

该镜像支持在容器启动阶段通过环境变量动态注入并覆盖 Headscale 的各项配置，重点支持对数据库 (Database) 信息的配置。

以下是支持的环境变量：

### 核心配置
- `HEADSCALE_SERVER_URL`: 暴露在公网的服务访问地址（例如 `https://mytailnet.com`），会覆盖内部配置的 `server_url`。
- `LOCAL_LOGIN_URL`: 本地 Tailscale 客户端注册使用的 URL（默认为内部地址）。

### 数据库配置 (Database)
为了便于对接外部持久化数据库，您可以通过配置 `HEADSCALE_DB_TYPE` 来切换数据库驱动。未配置时将默认使用内置的 `sqlite` 数据库。

支持的环境变量：
- `HEADSCALE_DB_TYPE`：数据库类型，可选值为 `postgres` 或 `sqlite`。若留空或不填，则默认为 `sqlite`。
- `HEADSCALE_DB_HOST`：数据库主机地址（当 TYPE 为 postgres 时可用，默认 `localhost`）。
- `HEADSCALE_DB_PORT`：数据库端口（当 TYPE 为 postgres 时可用，默认 `5432`）。
- `HEADSCALE_DB_NAME`：数据库名称（默认 `headscale`）。
- `HEADSCALE_DB_USER`：数据库用户名（默认 `headscale`）。
- `HEADSCALE_DB_PASS`：数据库密码（默认 `headscale`）。
- `HEADSCALE_DB_SSL_MODE`：Postgres 的 SSL 模式（默认 `disable`）。

**配置示例：**

```bash
docker run -d \
  -e HEADSCALE_DB_TYPE=postgres \
  -e HEADSCALE_DB_HOST=192.168.1.100 \
  -e HEADSCALE_DB_PORT=5432 \
  -e HEADSCALE_DB_NAME=headscale_db \
  -e HEADSCALE_DB_USER=admin \
  -e HEADSCALE_DB_PASS=secret \
  -e HEADSCALE_DB_SSL_MODE=disable \
  -p 8000:8000 \
  my-headscale-image
```

## 实现原理

在 `entrypoint.sh` 执行时：
1. 脚本会拦截启动流程并检测是否提供了 `$HEADSCALE_DB_TYPE` 环境变量。
2. 若检测到该变量，使用 `awk` 脚本动态覆写容器内部 `/etc/headscale/config.yaml` 文件的 `database:` 区块。
3. 若配置类型为 `sqlite`，则自动执行数据库文件初始化（`touch db.sqlite`）。
4. 随后通过 `headscale serve &` 拉起核心服务，再继续执行网络连接、路由配置和代理等环节。
