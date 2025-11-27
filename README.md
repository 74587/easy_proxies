# Easy Proxies

基于 [sing-box](https://github.com/SagerNet/sing-box) 的代理节点池管理工具，支持多协议、多节点自动故障转移和负载均衡。

## 特性

- **多协议支持**: VLESS、Hysteria2、Shadowsocks、Trojan
- **多种传输层**: TCP、WebSocket、HTTP/2、gRPC、HTTPUpgrade
- **节点池模式**: 自动故障转移、负载均衡
- **多端口模式**: 每个节点独立监听端口
- **Web 监控面板**: 实时查看节点状态、延迟探测
- **灵活配置**: 支持配置文件和节点文件分离

## 快速开始

### 1. 配置

复制示例配置文件：

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

编辑 `config.yaml` 配置监听地址和认证信息，编辑 `nodes.txt` 添加代理节点。

### 2. 运行

**Docker 方式（推荐）：**

```bash
./start.sh
```

或手动执行：

```bash
docker compose up -d
```

**本地编译运行：**

```bash
go build -tags "with_utls with_quic with_grpc" -o easy-proxies ./cmd/easy_proxies
./easy-proxies --config config.yaml
```

## 配置说明

### 基础配置

```yaml
mode: pool                    # 运行模式: pool (节点池)
log_level: info               # 日志级别: debug, info, warn, error

# 管理接口
management:
  enabled: true
  listen: 0.0.0.0:9090        # Web 监控面板地址
  probe_target: www.apple.com:80  # 延迟探测目标

# 统一入口监听
listener:
  address: 0.0.0.0
  port: 2323
  username: username
  password: password

# 节点池配置
pool:
  mode: sequential            # sequential (顺序) 或 random (随机)
  failure_threshold: 3        # 失败阈值，超过后拉黑节点
  blacklist_duration: 24h     # 拉黑时长

# 多端口模式
multi_port:
  address: 0.0.0.0
  base_port: 24000            # 起始端口，节点依次递增
  username: mpuser
  password: mppass
```

### 节点配置

**方式 1: 使用节点文件（推荐）**

在 `config.yaml` 中指定：

```yaml
nodes_file: nodes.txt
```

`nodes.txt` 每行一个节点 URI：

```
vless://uuid@server:443?security=reality&sni=example.com#节点名称
hysteria2://password@server:443?sni=example.com#HY2节点
ss://base64@server:8388#SS节点
trojan://password@server:443?sni=example.com#Trojan节点
```

**方式 2: 直接在配置文件中**

```yaml
nodes:
  - uri: "vless://uuid@server:443#节点1"
  - name: custom-name
    uri: "ss://base64@server:8388"
    port: 24001  # 可选，手动指定端口
```

## 支持的协议

| 协议 | URI 格式 | 特性 |
|------|----------|------|
| VLESS | `vless://` | Reality、XTLS-Vision、多传输层 |
| Hysteria2 | `hysteria2://` | 带宽控制、混淆 |
| Shadowsocks | `ss://` | 多加密方式 |
| Trojan | `trojan://` | TLS、多传输层 |

### VLESS 参数

```
vless://uuid@server:port?encryption=none&security=reality&sni=example.com&fp=chrome&pbk=xxx&sid=xxx&type=tcp&flow=xtls-rprx-vision#名称
```

- `security`: none, tls, reality
- `type`: tcp, ws, http, grpc, httpupgrade
- `flow`: xtls-rprx-vision (仅 TCP)
- `fp`: 指纹 (chrome, firefox, safari 等)

### Hysteria2 参数

```
hysteria2://password@server:port?sni=example.com&insecure=0&obfs=salamander&obfs-password=xxx#名称
```

- `upMbps` / `downMbps`: 带宽限制
- `obfs`: 混淆类型
- `obfs-password`: 混淆密码

## Web 监控面板

访问 `http://localhost:9090` 查看：

- 节点状态（健康/警告/异常/拉黑）
- 实时延迟
- 活跃连接数
- 失败次数统计
- 手动探测延迟
- 解除节点拉黑

## 端口说明

| 端口 | 用途 |
|------|------|
| 2323 | 统一代理入口（节点池模式） |
| 9090 | Web 监控面板 |
| 24000+ | 多端口模式，每节点独立端口 |

## Docker 部署

```yaml
# docker-compose.yml
services:
  easy-proxies:
    build: .
    image: easy-proxies:local
    container_name: easy-proxies
    restart: unless-stopped
    ports:
      - "2323:2323"
      - "9090:9090"
      - "24000-24100:24000-24100"  # 多端口模式
    volumes:
      - ./config.yaml:/etc/easy-proxies/config.yaml:ro
      - ./nodes.txt:/etc/easy-proxies/nodes.txt:ro
```

## 构建

```bash
# 基础构建
go build -o easy-proxies ./cmd/easy_proxies

# 完整功能构建
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies
```

## 许可证

MIT License
