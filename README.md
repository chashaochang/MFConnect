# MF Connect

为 Unraid 提供自部署的 HTTPS 中继和 Docker WebUI 访问，供 RaidMate 接入。手机使用普通 HTTPS，无需额外隧道客户端或 VPN。

## 使用已有中继

在 RaidMate 或 Unraid 的 **Settings → MF Connect** 中填写部署者提供的 HTTPS 服务入口，再设置自己的连接 ID。普通使用者不需要云账号、DNS 凭据或另购服务器。

## 自己部署中继

支持**普通 Docker** 和 **Docker Compose**，任选一种。中继服务和 HTTPS 网关运行在容器内，镜像从 GitHub 容器仓库 GHCR 拉取。

### 第一步：准备服务器、域名和 SSL 证书

- 一台安装了 Docker Engine 的 Linux amd64 服务器，云厂商不限；选择 Compose 时还需 Compose V2。
- 一个自己的域名，将服务入口及应用子域名解析到服务器公网 IP。
- 为这些域名准备好 **SSL 证书（也叫 HTTPS 证书）**，供手机和浏览器通过 HTTPS 访问。
- 放行 TCP **80、443、17000**。

**域名解析、SSL 证书的申请和续期由部署者自行管理，使用什么平台或工具由你决定。下面的安装方式不需要向 MF Connect 提供云账号或 DNS API 密钥。**

证书需要同时包含入口域名和它下面的通配子域名：

| 你打算使用的服务入口 | SSL 证书需要包含的域名 |
| --- | --- |
| `https://example.com` | `example.com` 和 `*.example.com` |
| `https://connect.example.com` | `connect.example.com` 和 `*.connect.example.com` |

`*` 用来覆盖自动分配给 NAS、Docker 应用的子域名，不需要每个应用单独申请证书。只包含 `*.example.com` 的证书不能覆盖 `example.com` 本身，也不能覆盖 `app.connect.example.com`。

### 第二步：填写入口并放入 SSL 证书文件

下面示例使用 MF Connect 自带的 HTTPS 网关，读取你已经申请好的证书。已有反向代理的部署要求见[接入自己的反向代理](docs/deployment.md#接入自己的反向代理)。

在服务器执行以下命令，将 `https://connect.example.com` 改成你要使用的入口：

```bash
mkdir -p mfconnect/config
cd mfconnect
cat > .env <<'ENV'
MF_ENTRY_ORIGIN=https://connect.example.com
MF_TLS_MODE=provided
ENV
chmod 600 .env
```

`MF_TLS_MODE=provided` 表示读取你提供的 SSL 证书文件。准备下面两个 **PEM 格式**的文件：

| 放到服务器上的位置 | 文件内容 |
| --- | --- |
| `mfconnect/config/fullchain.pem` | SSL 证书，包含站点证书和中间证书，也就是“完整证书链” |
| `mfconnect/config/privkey.pem` | 该证书配套的私钥，使用无密码版本 |

在刚才创建的 `mfconnect` 目录内执行，将前两个路径换成你实际保存文件的位置：

```bash
cp "/你的证书目录/证书文件.pem" config/fullchain.pem
cp "/你的证书目录/私钥文件.key" config/privkey.pem
chmod 600 config/privkey.pem
```

`chmod 600` 用于限制私钥文件的读写权限。原文件叫什么名字不影响使用，但内容必须是 PEM 格式；只修改扩展名不能转换证书格式。续期后把新证书和私钥更新到这两个位置，网关会校验并自动加载。

### 第三步：启动服务（二选一）

#### 方式一：普通 Docker

直接使用 `docker run`。两个容器分别负责 HTTPS 网关和中继服务，共享证书和插件下载目录。

首次安装创建专用网络和数据卷：

```bash
docker network create --subnet 172.31.253.0/28 --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-network
docker volume create --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-gateway
docker volume create --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-tls
docker volume create --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-updates
docker volume create --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-data
```

启动网关：

```bash
docker run -d --name mfconnect-docker-gateway \
  --label io.mfconnect.docker-project=mfconnect-docker \
  --platform linux/amd64 --init --restart unless-stopped --stop-timeout 120 \
  --read-only --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
  --cap-add SETUID --cap-add SETGID --cap-add NET_BIND_SERVICE --security-opt no-new-privileges:true \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m,mode=1777 --tmpfs /run:rw,noexec,nosuid,size=8m,mode=0755 \
  --network mfconnect-docker-network --ip 172.31.253.2 --network-alias gateway \
  -p 80:80 -p 443:443 --env-file .env \
  -v mfconnect-docker-gateway:/state -v mfconnect-docker-tls:/state/certs \
  -v mfconnect-docker-updates:/updates -v "$PWD/config:/config:ro,z" \
  --health-cmd 'python3 /opt/mfconnect/gateway.py health' \
  --health-interval 10s --health-timeout 5s --health-retries 60 \
  --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 \
  ghcr.io/chashaochang/mfconnect-gateway:0.8.1
```

执行 `docker ps`，等待网关显示 **healthy** 后启动中继服务。如果未就绪，用 `docker logs --tail=100 mfconnect-docker-gateway` 检查证书或 DNS 配置。

```bash
docker run -d --name mfconnect-docker-server \
  --label io.mfconnect.docker-project=mfconnect-docker \
  --platform linux/amd64 --init --restart unless-stopped --stop-timeout 60 \
  --user 10001:10001 --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m,mode=1777 \
  --network mfconnect-docker-network --network-alias mfconnect \
  -p 17000:17000 --env-file .env \
  -e 'MF_TRUSTED_PROXY_IPS=["172.31.253.2"]' -e MF_WATCH_TLS=1 \
  -e MF_UPDATE_DIR=/var/lib/mfconnect-updates \
  -e MF_PLUGIN_MANIFEST=/var/lib/mfconnect-updates/plugin-install.plg \
  -e MF_INSTALL_MODE=self_service -e MF_INSTALL_DEVICE_LIMIT=10 \
  -v mfconnect-docker-data:/var/lib/mfconnect -v mfconnect-docker-tls:/etc/mfconnect/tls:ro \
  -v mfconnect-docker-updates:/var/lib/mfconnect-updates \
  --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 \
  ghcr.io/chashaochang/mfconnect-server:0.8.1-docker.1 bootstrap

docker ps
```

#### 方式二：Docker Compose

在准备好 `.env` 和 `config/` 的部署目录下载完整配置，然后启动：

```bash
curl -fL --proto '=https' --proto-redir '=https' --retry 3 \
  https://raw.githubusercontent.com/chashaochang/MFConnect/v0.8.1/compose.yaml \
  -o compose.yaml
docker compose up -d --wait --wait-timeout 660
docker compose ps
```

完整配置见 [compose.yaml](compose.yaml)，可直接查看或复制。首次启动自动拉取两个镜像，Compose 会等待网关就绪后启动服务端。查看日志使用 `docker compose logs --tail=100`。

镜像仓库无法访问时可用[离线镜像包](docs/deployment.md#离线镜像包)。更新、备份及两种方式的管理命令见[部署与维护](docs/deployment.md)。

## 安装 Unraid 插件

1. 在 Unraid 7+ 打开 **Plugins → Install Plugin**。
2. 粘贴下面的 GitHub 官方安装地址，点击安装：

   ```text
   https://github.com/chashaochang/MFConnect/releases/latest/download/mfconnect.plg
   ```

3. 打开 **Settings → MF Connect**，填写自己的 HTTPS 中继入口和连接 ID；也可在支持本协议的 RaidMate 中配置。

已部署中继也会提供 `https://你的入口/downloads/mfconnect/mfconnect.plg`，原安装方式继续可用。GitHub 安装地址跟随项目最新正式版；中继提供的地址跟随该部署的插件版本。插件安装后可在 Unraid 插件管理器中检查更新，安装来源不影响已有配置。

支持自动安装的客户端也可取得一次性安装链接，无需邀请码或云账号。开启 MF Connect 后，所有已声明 WebUI 且目标有效的 Docker 应用会自动接入，新增或重建应用也会自动关联，无需逐个发布。关闭 MF Connect 后远程访问关闭；中继认证与应用本身的登录仍然有效。

## Release 下载文件怎么选

[GitHub Releases](https://github.com/chashaochang/MFConnect/releases/latest) 同时提供服务器部署文件和 Unraid 插件，版本号用 `<版本>` 表示：

| 你要安装什么 | 使用哪个文件 |
| --- | --- |
| 云服务器，镜像在线拉取 | `mfconnect-<版本>-deploy.tar.gz`，支持 Docker / Compose |
| 云服务器，需要离线镜像 | `mfconnect-<版本>-docker-amd64.tar.gz` |
| Unraid 插件 | 在“安装插件”中粘贴上面的 `mfconnect.plg` 地址 |
| 插件程序包 | `mfconnect-<版本>-linux-amd64.tar.gz`，由 `.plg` 自动下载，无需手动解压 |

`SHA256SUMS` 校验服务器部署包，`PLUGIN-SHA256SUMS` 校验插件文件。`bundle.json` 和 `plugin-release.json` 分别记录服务器与插件的版本信息。GitHub 自动生成的 **Source code** 是源码，不是安装包；此仓库不发布 RaidMate 手机客户端。

## 地址与能力

| 用途 | 入口为 `https://example.com` 时 |
| --- | --- |
| 控制 API | `https://example.com/api` |
| 连接 ID 入口 | `https://example.com/home` |
| NAS 原生登录 | `https://home.example.com` |
| Docker WebUI | `https://app-<自动生成的稳定标识>.example.com` |

入口也可以是子域名。每个 NAS 和应用保持独立来源，控制 Token 不进入 WebView。保留旧双域名部署的兼容接口；配置签名发行频道后可通过 App 检查并执行更新。普通 LAN/DDNS 登录可独立使用。

- [部署与维护](docs/deployment.md)：手动部署、DNS、证书、更新、备份和迁移。
- [API 与客户端接入](docs/api.md)：端点契约、鉴权、应用目录、安装及更新接口。
- [开发说明](docs/development.md)：构建、测试和发布范围。
