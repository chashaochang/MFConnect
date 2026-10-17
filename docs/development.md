# 开发

## 源码结构

| 目录 | 内容 |
| --- | --- |
| `cmd/` | 云端、NAS 连接程序和更新执行器 |
| `internal/` | 鉴权、隧道、发现、代理、持久化及测试 |
| `plugin/` | Unraid 安装脚本、设置页和客户端接入端点 |
| `deploy/` | 通用 Docker、systemd 和 DNS/TLS 模板 |
| `scripts/` | 可重复执行的构建、打包和隔离测试 |
| `docs/` | 使用者和接入开发者需要的接口、部署及开发说明 |

## 构建与测试

Go 版本见 `go.mod`。Python 工具使用 Python 3；部署测试还需要 PyYAML、OpenSSL、Nginx，插件测试使用 PHP 和 jq。FRP 固定版本及校验在 `scripts/build_frp.sh` 中维护。

```bash
make build
make frp
go test -race ./internal/... ./cmd/...
python3 scripts/test_plugin.py
python3 scripts/test_compose_installer.py
python3 scripts/test_unified_deployment.py
python3 scripts/test_unified_deployment.py --native
```

`--native` 使用临时目录、测试 CA、本机端口和真实 Nginx/frpc/frps。需要允许本机监听，不需要生产域名或 NAS 凭据。`scripts/test_docker.py` 验证原生入口行为；`scripts/test_cloud_container.py` 在有 Docker 的测试主机上验证容器隔离和持久化。

本地测试不能代替公网证书、真实 NAS、客户端登录或媒体播放验收。实际环境的测试结果留在运行环境中，不写进公共 README。

## 打包

```bash
make plugin-local
make docker-bundle
make plugin RELEASE_BASE=https://downloads.example.com/mfconnect
```

`plugin-local` 生成离线测试包；在线清单引用指定发布地址。`docker-bundle` 生成 Compose 构建目录包，不是可直接 `docker load` 的镜像归档。版本、载荷摘要和签名必须与实际程序一致。

完整 Docker 部署测试为 `sudo python3 scripts/test_compose_stack.py`，使用独立项目、数据卷、测试 CA 和示例域名，覆盖无云凭据启动、证书轮换和配置保留。

GitHub 的 `v*` 发行标签触发 `.github/workflows/release.yml`：执行检查、编译程序、构建镜像和插件、测试真实 Docker / Nginx，再发布部署包和 SHA-256 清单。`scripts/package_release.py` 使用明确文件名单打包，不复制整个工作目录。第三方 acme.sh 固定摘要并保留完整源码和许可证。发布后的固定版本文件不覆盖，修订使用新标签和新文件名。

## 仓库与本地文件

Git 管理通用源码、测试、依赖锁定文件、构建脚本、示例配置和上述公共文档。示例只使用保留的示例域名或测试地址，不复制实际部署配置。

`.local/` 用于本地运维说明和交接，`dist/` 用于构建产物和测试输出。真实 `.env`、凭据、证书私钥、数据库、设备身份、备份、SSH 配置和会话记录均不提交。它们也不因被 `.gitignore` 忽略就适合复制到公开下载目录。

发布前检查待发布文件、提交历史和作者信息，确认依赖许可及项目许可证。`scripts/check_publication.py` 可使用本地私密拒绝列表检查历史中的实际部署信息；检查通过后仍需审查公开文档和归档内容。
