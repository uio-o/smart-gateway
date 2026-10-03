# smart-gateway

一个自托管的动态多线路 API 流量网关。首访自动选一条最快的线路，同一会话固定在这条线路上；只有线路真的变差才切换。

它解决的具体问题：从境内访问境外 API 时，直连质量不稳定。你手上有几个中转节点（东京、香港、BGP……），但：

- 不知道此刻哪条最快
- 手工切线路会让正在进行的对话断掉
- SSE / WebSocket 长连接一断就得重来

smart-gateway 把这些交给一个入口节点自动完成。

```text
你的客户端
  ↓ HTTPS  ai.example.com（你自己已备案的域名）
你的反向代理（1Panel / Nginx / Caddy）
  │  · 证书、TLS 终止
  │  · 路径白名单、鉴权
  ↓ 127.0.0.1:8080
入口 Agent（entry）
  │  · 选路（健康度优先）
  │  · 会话黏性
  │  · 故障转移
  │  · SSE / WebSocket
  ↓ IP:3001
中转 Agent（relay）→ Aether / New API / 任何 HTTP 源站
```

## 两个组件

| 组件 | 部署位置 | 职责 |
|---|---|---|
| **面板（panel）** | 境外一台机器 | 存拓扑、分配端口、生成 Agent 配置、看心跳 |
| **Agent（agent）** | 每个节点 | 入口节点选路转发；中转节点纯 TCP 转发 |

面板是唯一的控制面。Agent 主动来拉配置，因此**节点不需要对外开放任何管理端口**，也不需要域名和证书。

## 设计要点

**入口用域名，中转只用 IP。** 只有客户端需要按域名访问。节点之间是 Agent 之间 IP 直连，不需要 DNS，不需要证书。

**Agent 不碰域名和证书。** 入口 Agent 只监听 `127.0.0.1:8080`，TLS、证书续期、路径白名单全交给你的反向代理——你已经会用 1Panel 管这些了。

**自己写转发器而不是套 Nginx/Envoy。** 改路由表不能断 SSE。`nginx reload` 会掐掉正在流式返回的连接；一个 Go 进程换掉内存里的路由表不会。

**配置全部可配，不写死。** 入口路径前缀、路径白名单、源站、允许方法都在面板里改，改完热加载。代码里没有一条写死的业务路径。

**健康度优先于配置顺序。** 主线路坏掉时不会被"顺序"遮蔽，备用线路会顶上来。这是本项目修掉的一个真实缺陷。

**校验函数无副作用。** `config.Validate()` 不改写文档。面板和 Agent 用同一份校验逻辑，如果校验会改内容，两边就会对同一份文档产生不同理解（曾经因此让面板覆盖了每个入口节点的本机监听地址）。

## 快速开始
## 版本与升级

Agent 和面板可以**独立升级**，因为它们用各自的 tag 发布：

| Tag 形态 | 构建内容 | 用途 |
|---|---|---|
| `agent-v0.1.1` | 只构建 Agent | 节点单独升级 Agent |
| `panel-v0.1.1` | 只构建面板 | 单独升级控制面 |
| `v0.1.1` | 两者都构建 | 协调发布 |

带 `-rc1`、`-beta.1` 这类后缀的 tag 会标记为 **pre-release**，不会被当成当前稳定版。

每次发布都是**完整产物**（不发行补丁）：二进制、校验和、容器镜像一整套。

### 升级 Agent（节点上）

```bash
# 下载新版本并校验
V=0.1.1
curl -fsSL -o /tmp/sg-agent \
  "https://github.com/uio-o/smart-gateway/releases/download/agent-v${V}/smart-gateway-agent-linux-amd64"
curl -fsSL -o /tmp/sg-sums \
  "https://github.com/uio-o/smart-gateway/releases/download/agent-v${V}/checksums-linux-amd64.txt"
(cd /tmp && grep smart-gateway-agent-linux-amd64 sg-sums | sed 's|smart-gateway-agent-linux-amd64|sg-agent|' | sha256sum -c)

# 替换并重启，配置由面板下发，不需要改本地文件
install -m 755 /tmp/sg-agent /opt/smart-gateway/bin/smart-gateway-agent
systemctl restart smart-gateway-agent
```

Agent 重启后会从面板重新拉取配置，期间不改变任何服务端状态。

### 升级面板

```bash
docker compose pull panel
docker compose up -d panel
```

### 容器镜像

镜像发布在 GHCR，Agent 与面板是**两个独立镜像**，节点只需拉取自己要跑的那个：

```
ghcr.io/uio-o/smart-gateway/agent:0.1.1
ghcr.io/uio-o/smart-gateway/panel:0.1.1
```


### 1. 起面板

```bash
git clone https://github.com/uio-o/smart-gateway
cd smart-gateway
cp .env.example .env

# 生成面板令牌
openssl rand -hex 32   # 填进 .env 的 PANEL_ADMIN_TOKEN

docker compose up -d
```

面板默认只监听 `127.0.0.1:8090`，请放在你自己的反向代理后面。启动后打开面板（浏览器输入你的面板地址），用令牌登录。

### 2. 在面板里描述拓扑

1. **节点**：加一个中转节点，地址填 `IP:3001`（该节点上转发端口）
2. **服务**：加源站，填 `http://你的Aether:8084` 和入口路径前缀（比如 `/v1`）
3. **路由**：加一条路由，跳点选刚加的中转节点，端口留空自动分配
4. **接入令牌**：选中转节点，生成令牌——面板会给出完整的安装命令
5. **调度设置**：模式选 `primary_backup`，主线路选刚建的路由

### 3. 装中转节点

在中转机器上以 root 执行面板给出的命令：

```bash
curl -fsSL https://你的面板/install.sh | sudo bash -s -- \
  --panel https://你的面板 \
  --node 节点名 \
  --role relay \
  --token sg_xxx
```

脚本会装一个静态二进制 + systemd 服务。中转节点**不需要**域名、证书、Nginx。

防火墙只需对其他网关节点放行转发端口段（默认 3001-3100）。

### 4. 装入口节点

```bash
curl -fsSL https://你的面板/install.sh | sudo bash -s -- \
  --panel https://你的面板 \
  --node 入口名 \
  --role entry \
  --listen 127.0.0.1:8080 \
  --token sg_yyy
```

然后在 1Panel / Nginx 里加一个反代：`ai.example.com` → `http://127.0.0.1:8080`。

路径白名单在反代里配（例如只放行 `/v1/*`），入口节点不用管。

### 5. 验证

```bash
curl -X POST https://ai.example.com/v1/messages -d '{"hello":"world"}'
```

面板的「概览」页会显示节点在线、配置哈希和活跃连接数。

## 手动部署（不用 Docker）

```bash
go build -o smart-gateway-panel ./cmd/smart-gateway-panel
go build -o smart-gateway-agent ./cmd/smart-gateway-agent

# 面板
./smart-gateway-panel -addr 127.0.0.1:8090 -data /var/lib/smart-gateway \
  -token-file /etc/smart-gateway/admin.token

# 入口 Agent（托管模式：从面板拉配置）
./smart-gateway-agent -panel https://你的面板 -node 入口名 -role entry \
  -listen 127.0.0.1:8080 -panel-token sg_yyy

# 中转 Agent
./smart-gateway-agent -panel https://你的面板 -node 节点名 -role relay \
  -panel-token sg_xxx
```

也支持完全脱离面板的本地文件模式：

```bash
./smart-gateway-agent -config agent.yaml -print-config   # 校验并打印摘要
./smart-gateway-agent -config agent.yaml
```

参考 [`examples/relay.yaml`](examples/relay.yaml)。

## 配置项

Agent 的托管模式只需要身份信息，其余全部来自面板：

| 参数 | 说明 |
|---|---|
| `-panel` | 面板地址，填了就启用托管模式 |
| `-node` | 面板里注册的节点名 |
| `-role` | `entry` 或 `relay` |
| `-panel-token` | 面板签发的 Agent 令牌 |
| `-listen` | 入口节点的本机监听地址，默认 `127.0.0.1:8080` |

面板参数：

| 参数 | 说明 |
|---|---|
| `-addr` | 面板监听地址，默认 `:8090` |
| `-data` | SQLite 数据目录 |
| `-token-file` | 面板令牌文件（比环境变量更安全，不出现在进程列表） |
| `-mount-prefix` | 把面板挂在子路径下 |
| `-agent-dir` | 本地 Agent 二进制目录（节点上不了外网时用） |
| `-release-base` | Agent 二进制的下载源前缀 |

面板的调度设置里可以调：

- **路由模式**：`primary_backup`（主备）、`static`（固定）、`weighted`（加权）
- **会话黏性**：保持时长、失败阈值
- **主动探测**：间隔、超时、样本数
- **限流**：最大并发、每分钟请求上限
- **端口池**：转发端口自动分配范围
- **入口监听地址**：留空则由各节点自己决定（推荐）

## 面板 API

面板 UI 和 API 用的是同一套东西，可以脚本化。

Agent 接口（用 Agent 令牌）：

```text
GET  /api/agent/config?node=<name>     # 拉配置
POST /api/agent/heartbeat              # 上报心跳
```

运维接口（用面板令牌，`Authorization: Bearer <token>` 或 `X-Panel-Token`）：

```text
GET    /api/summary
GET    /api/nodes               POST /api/nodes
GET    /api/nodes/<id>          PUT  /api/nodes/<id>       DELETE /api/nodes/<id>
GET    /api/services            POST /api/services
GET    /api/services/<id>       PUT  /api/services/<id>    DELETE /api/services/<id>
GET    /api/routes              POST /api/routes
GET    /api/routes/<id>         PUT  /api/routes/<id>      DELETE /api/routes/<id>
GET    /api/tokens              POST /api/tokens
DELETE /api/tokens/<id>
GET    /api/settings            PUT  /api/settings
GET    /api/render?role=&node=  # 预览生成的 Agent 配置
GET    /healthz                 # 无需鉴权
```

## 监控

入口节点本地暴露两个只读端点，不经过转发：

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/_gateway/stats
```

`stats` 给出每条线路的样本数、成功率、p50/p95 延迟、连续失败次数和健康判定。

开启审计日志后，每条请求会写一行 JSON（客户端 IP、路径、服务、选中线路、选择原因、是否故障转移、会话键、状态码、耗时、响应字节数）。在面板的调度设置里填 `audit_path` 即可。

## 开发

```bash
go build ./...
go vet ./...
go test ./...

# 端到端冒烟测试（含故障转移验证）
bash scripts/smoke.sh
```

发布：打 `v*` 标签后 GitHub Actions 会为 linux/amd64 和 linux/arm64 构建 Agent 与面板二进制，面板的 `/download/` 端点会代理这些产物，所以 `install.sh` 只需要面板一个来源。

## License

MIT
