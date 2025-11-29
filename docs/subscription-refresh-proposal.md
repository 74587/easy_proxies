# 订阅定时刷新方案

## 背景

当前 easy_proxies 支持从订阅链接获取节点，但只在启动时加载一次。用户需要定时刷新订阅以获取最新节点，同时不能中断现有连接。

## 需求

1. **定时刷新订阅**：按配置的间隔自动从订阅链接获取最新节点
2. **热更新 outbound**：刷新后真正更新代理出口，而不是只更新配置
3. **最小化中断**：更新过程中尽量减少对现有连接的影响
4. **健康检查**：新节点需要通过健康检查后才能投入使用

## 技术方案：优雅重启

### 核心思路

采用"先建后拆"的策略：
1. 获取新的订阅节点
2. 创建新的 sing-box 实例（使用新配置）
3. 对新实例的节点进行健康检查
4. 健康检查通过后，切换流量到新实例
5. 等待旧实例的活跃连接结束（设置超时）
6. 关闭旧实例

### 架构设计

```
┌─────────────────────────────────────────────────────────────┐
│                      SubscriptionManager                     │
│  - 定时获取订阅                                               │
│  - 解析节点配置                                               │
│  - 触发重载                                                   │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                       BoxManager                             │
│  - 管理 sing-box 实例生命周期                                 │
│  - 实现优雅切换                                               │
│  - 跟踪活跃连接                                               │
└─────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┴───────────────┐
              ▼                               ▼
┌─────────────────────────┐     ┌─────────────────────────┐
│    Old Box Instance     │     │    New Box Instance     │
│    (draining)           │     │    (active)             │
└─────────────────────────┘     └─────────────────────────┘
```

### 配置项

```yaml
# 订阅刷新配置
subscription_refresh:
  enabled: true
  interval: 1h              # 刷新间隔，默认 1 小时
  timeout: 30s              # 获取订阅的超时时间
  health_check_timeout: 60s # 新节点健康检查超时
  drain_timeout: 30s        # 旧实例排空超时时间
  min_available_nodes: 1    # 最少可用节点数，低于此值不切换
```

### 实现步骤

#### 阶段 1：SubscriptionManager

```go
type SubscriptionManager struct {
    cfg           *config.Config
    interval      time.Duration
    onRefresh     func(nodes []config.NodeConfig) error
    ctx           context.Context
    cancel        context.CancelFunc
    logger        Logger
}

// Start 启动定时刷新
func (m *SubscriptionManager) Start()

// Stop 停止定时刷新
func (m *SubscriptionManager) Stop()

// RefreshNow 立即刷新（可通过 API 触发）
func (m *SubscriptionManager) RefreshNow() error

// fetchSubscriptions 获取所有订阅的节点
func (m *SubscriptionManager) fetchSubscriptions() ([]config.NodeConfig, error)
```

#### 阶段 2：BoxManager

```go
type BoxManager struct {
    mu            sync.RWMutex
    currentBox    *box.Box
    currentCfg    *config.Config
    monitorMgr    *monitor.Manager
    drainTimeout  time.Duration
    logger        Logger
}

// Reload 使用新配置重载
func (m *BoxManager) Reload(newCfg *config.Config) error

// gracefulSwitch 优雅切换
func (m *BoxManager) gracefulSwitch(newBox *box.Box, newMonitor *monitor.Manager) error

// drainOldBox 排空旧实例
func (m *BoxManager) drainOldBox(oldBox *box.Box, timeout time.Duration)
```

#### 阶段 3：健康检查集成

```go
// 在切换前进行健康检查
func (m *BoxManager) preflightCheck(newMonitor *monitor.Manager) error {
    // 1. 等待初始健康检查完成
    // 2. 检查可用节点数是否满足最低要求
    // 3. 如果不满足，回滚到旧配置
}
```

### 切换流程

```
时间线:
────────────────────────────────────────────────────────────────►

T0: 触发刷新
    │
    ▼
T1: 获取订阅节点
    │
    ▼
T2: 创建新 Box 实例（此时旧实例仍在服务）
    │
    ▼
T3: 新实例健康检查
    │
    ├── 失败 → 销毁新实例，保持旧实例，记录错误
    │
    └── 成功 ↓
              │
T4: 切换入口（新请求走新实例）
    │
    ▼
T5: 旧实例进入 draining 状态
    │  - 不再接受新连接
    │  - 等待现有连接结束
    │  - 超时后强制关闭
    │
    ▼
T6: 关闭旧实例，切换完成
```

### 端口处理（多端口模式）

多端口模式下，每个节点有独立端口，切换时需要特殊处理：

**方案 A：端口复用**
- 新旧实例使用相同端口
- 需要先关闭旧端口，再开启新端口
- 会有短暂的端口不可用时间

**方案 B：端口偏移**
- 新实例使用不同的端口范围（如 base_port + 1000）
- 切换时更新导出的端口信息
- 无端口冲突，但客户端需要更新配置

**推荐方案 A**，因为：
- 客户端无需更新配置
- 中断时间很短（毫秒级）
- 实现相对简单

### API 接口

```
POST /api/subscription/refresh
  触发立即刷新订阅

GET /api/subscription/status
  获取订阅状态（上次刷新时间、节点数等）

GET /api/reload/status
  获取重载状态（是否正在重载、进度等）
```

### 错误处理

| 场景 | 处理方式 |
|------|----------|
| 订阅获取失败 | 记录错误，保持现有配置，下次重试 |
| 新实例创建失败 | 记录错误，保持现有配置 |
| 健康检查失败（可用节点不足） | 销毁新实例，保持现有配置 |
| 排空超时 | 强制关闭旧实例的剩余连接 |

### 日志输出

```
[subscription] 开始刷新订阅...
[subscription] 从 2 个订阅源获取到 50 个节点
[reload] 创建新实例...
[reload] 新实例健康检查: 45/50 节点可用
[reload] 开始切换流量...
[reload] 旧实例进入排空状态，等待 10 个活跃连接结束
[reload] 切换完成，耗时 2.5s
```

## 风险评估

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| 新配置有问题导致服务不可用 | 高 | 健康检查 + 最小可用节点数检查 |
| 切换过程中短暂中断 | 中 | 优化切换速度，预热新实例 |
| 内存占用增加（双实例） | 低 | 切换完成后立即释放旧实例 |
| 订阅源不稳定 | 中 | 重试机制 + 保持现有配置 |

## 开发计划

### 里程碑

1. **M1: 基础框架**（2-3 天）
   - SubscriptionManager 基本实现
   - 定时刷新逻辑
   - 配置项支持

2. **M2: BoxManager**（3-4 天）
   - Box 实例管理
   - 优雅切换逻辑
   - 连接排空

3. **M3: 健康检查集成**（1-2 天）
   - 预检查逻辑
   - 回滚机制

4. **M4: API 和测试**（2-3 天）
   - API 端点
   - 单元测试
   - 集成测试

### 分支策略

```
main
  │
  └── feature/subscription-refresh
        │
        ├── feat/subscription-manager
        ├── feat/box-manager
        ├── feat/health-check-integration
        └── feat/api-endpoints
```

## 替代方案

### 方案 B：进程级重启

使用外部进程管理器（如 systemd、supervisor）实现优雅重启：

```bash
# 发送 SIGHUP 信号触发重载
kill -HUP $(pidof easy-proxies)
```

**优点**：实现简单
**缺点**：仍有短暂中断，依赖外部工具

### 方案 C：只更新节点状态

不真正添加新节点，只定时检查现有节点可用性：

**优点**：实现最简单，无中断
**缺点**：新节点不会生效，需要手动重启

## 结论

推荐采用**优雅重启方案**，在可接受的短暂中断（毫秒级）内实现订阅热更新。该方案在复杂度和用户体验之间取得了较好的平衡。

## 待确认事项

1. [ ] 刷新间隔默认值（建议 1 小时）
2. [ ] 最小可用节点数默认值（建议 1）
3. [ ] 排空超时默认值（建议 30 秒）
4. [ ] 是否需要 API 触发刷新功能
5. [ ] 是否需要 WebUI 显示订阅状态
