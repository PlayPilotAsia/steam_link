# steam_link

**PlayPilot** 项目的 Steam 连接功能服务模块。

PlayPilot 是一个面向 PC 玩家的游戏平台，提供「游戏情报聚合 + 攻略检索 + AI 问答 + Steam 游戏画像 + 社区」五大能力。本仓库承担其中 **Steam 账号关联与游戏数据采集** 的职责，是「Steam 游戏画像」的数据底座——上层的 AI 问答、攻略推荐与社区展示都依赖本模块产出的游戏库、成就与游玩会话数据。

## 模块职责

| 编号 | 能力 | 说明 |
|---|---|---|
| F1 | 账号关联 | 通过 Steam 官方 OpenID 2.0 授权，将 Steam 账号绑定到 PlayPilot 账号 |
| F2 | 游戏库列表 | 用户拥有的全部游戏，含名称、图标、累计时长、最后游玩时间 |
| F3 | 游戏成就 | 全库完整成就数据：成就定义、用户解锁状态与解锁时刻、全球解锁率 |
| F4 | 时长监控 | 持续监控游戏行为，产出「何时玩了哪款游戏、玩了多久」的会话事件流 |

这些数据共同构成用户的 **Steam 游戏画像**：品类偏好、活跃时段、投入深度、成就完成度，供 PlayPilot 上层的情报推送与 AI 问答消费。

## 在 PlayPilot 中的位置

```
PlayPilot
├── 游戏情报聚合
├── 攻略检索
├── AI 问答
├── Steam 游戏画像  ←── 本模块（steam_link）提供数据
└── 社区
```

## 技术栈

| 层 | 选型 |
|---|---|
| 语言 | Go |
| HTTP 框架 | Gin |
| ORM | GORM |
| 主存储 | MySQL 8（utf8mb4） |
| 缓存与限流 | Redis 7 |
| 异步任务 | 本地事务表 + 定时扫描补偿（不引入消息队列） |
| 配置 | YAML 分环境 + viper，敏感项由环境变量注入 |
| 日志 | 标准库 `log/slog` 结构化日志 |

## 采集架构

Steam 不提供任何推送机制，时长监控本质是「定期采样 + 快照差分」。采集分四层，由上一层观测到的**变化**驱动：

| 层 | 接口 | 触发方式 | 职责 |
|---|---|---|---|
| L0 | `GetPlayerSummaries` | 按 tier 定时，批量 100 人/次 | 检测「谁正在玩什么」 |
| L1 | `GetRecentlyPlayedGames` | 会话结束事件，延迟 5 分钟 | 获取精确时长增量，落会话 |
| L2 | `GetPlayerAchievements` | 该游戏时长有增量时 | 同步成就解锁 |
| L3 | `GetOwnedGames` | 每用户每日一次 | 同步游戏库 + 兜底补漏 |

L0 的批量能力是成本支点：1000 个用户全量探测一轮仅需 10 次调用。稳态日调用量约 3,300 次，占配额（100,000/天）的 3.3%。

## 目录结构

```
cmd/
  api/            Gin HTTP 服务：OpenID 登录、数据查询接口
  worker/         采集进程，可多实例水平扩展
configs/          分环境 YAML 配置
scripts/db/       初始化 DDL 与增量迁移脚本
internal/
  config/         viper 加载与校验
  logging/        slog Logger 构造
  steam/          Steam Web API 客户端 —— 唯一对外发起请求的包
  domain/         会话状态机与业务规则（纯函数，无 IO）
  collector/      L0–L3 各层 job 处理器
  task/           本地事务表：入队、扫描、租约、退避重试
  auth/           OpenID 2.0 验证 + Redis session
  store/          GORM repository
  api/            Gin handler 与 DTO
```

## 配置

敏感项一律不写入 YAML，由环境变量在部署时注入，缺失时启动即失败：

| 配置键 | 环境变量 |
|---|---|
| `steam.api_key` | `STEAMLINK_STEAM_API_KEY` |
| `mysql.password` | `STEAMLINK_MYSQL_PASSWORD` |
| `auth.state_secret` | `STEAMLINK_AUTH_STATE_SECRET` |

环境由 `APP_ENV` 选择（默认 `dev`）。

## 已知约束

- **OpenID 不返回访问凭证**：绑定的技术含义是「验证该 SteamID 归属该用户」，而非获得数据访问授权。所有数据仍以服务端自己的 API Key 读取该 SteamID 的**公开**数据。
- **隐私墙是静默的**：游戏详情非公开时 Steam 返回 HTTP 200 空对象而非错误码，必须在绑定时同步探测并向用户给出明确引导。
- **存在不可消除的精度损失**：短于探针间隔的会话、隐身用户的会话由每日校准推断补齐，并以 `source=reconcile` 标记，在数据模型层面与实测数据严格区分。

## 文档

- [设计文档](docs/01-design.md) — 平台约束、数据模型、采集管线、可靠性设计
- [实施文档](docs/02-implementation.md) — 分阶段落地细节
