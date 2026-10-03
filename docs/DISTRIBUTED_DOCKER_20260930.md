# Codex Agent 双节点与 Sub2API 同机入口

Sub2API 的两个节点现已各运行一个 Codex Agent。账号、设置、Key、1小时有界续聊缓存、并发租约使用主节点同一 PostgreSQL/Redis；没有复制数据库或永久保存对话。不同节点可用 previous_response_id 接续同一账号的历史，已真实验证。

两个 Sub2API 容器均使用同一个 Base URL：

    http://172.18.0.1:18879

OpenAI SDK 要求包含版本前缀时使用 /v1。此地址在各自 Docker verify_default 网络中指向本机宿主机，HTTP推理不再跨主机。主节点43.225.196.170的稳定入口18879经现有NAT到18878；peer43.225.196.214只监听Docker网关172.18.0.1:18879，不新增公网入口。

主备并非只读：两个应用节点均可接收请求。peer通过专用受限SSH通道连接主机状态端口127.0.0.1:25433和26380。两台服务器没有已连通物理私网或mesh，跨主机状态同步仍使用加密链路；这是共享一致性所需的小型数据访问，HTTP生成/流式转发在本机。

## 运行布局

- 每节点 /var/lib/codex-agent-release/active-runtime.json 记录自己的active/draining代。
- 每节点 codex-agent-ingress.service 保存稳定入口与Unix控制socket，不随应用热更新迁移。
- peer新增 codex-agent-state-tunnel.service，连接本机loopback数据库/Redis入口；禁止密码登录、shell、远程转发及其它目标端口。
- peer应用PG_MAX_CONNS=4。后续部署必须读取实际PG角色和总连接限制，不根据历史数值增加连接。
- 静态native HTTP helper与身份校验所需CLI由主机直接传给peer并验证SHA，不复制本地会话工作目录。运行时identity文件通过硬链接复用CLI，不多留一份269MB文件。
- 两节点保持同一加密key/namespace/storage identity，禁止在peer初始化新账号库或Redis。

## 发布

主节点继续用已有performance_release上传及hot_release热切。peer使用.local/error-repair-20260930/roll_peer.py：主节点直接传不可变release，验证fresh只读共享schema和存储identity，peer stage→ready→原子切稳定入口→等旧连接自然排空。不得直接重启持有请求/WS的旧代。

peer没有数据库Docker容器，因此stage的schema检查读取主节点共享数据库的实时验证结果；不修改不可变包中的控制器文件。

## 验证

两台Sub2API真实network namespace内 /health/ready、带鉴权/v1/models均200，models返回6项（原生返回字段models）。真实两轮生成从主节点开始、peer接续，标记回复正确、账号归属一致；TTL保留1小时、缓存总正文不超过512MiB、没有PG永久历史写入。

缓存率控制已在两节点同步。用户设置85%时，第二轮input_tokens=37290、cached_tokens=31696，等于向下取整的85%；系统原始用量独立保留。

证据在.local/error-repair-20260930/peer-verified-113144.json及cross-node-continuation-113507.json。后续运行版本以各节点registry和/proc/PID/exe SHA为准。

## 本轮最终版本

2026-09-30 19:42 CST，两节点当前binary均为 `814ea54328a9aa411820f78ffdd7417017b6f4636fce0d6a9ecfb34f15bcc60b`，release为 `4bc30e5f802fb038304b97f4b7caf7f0abdbfa3ae650284245d2409072b7367a`。共享设置版本42，用户缓存读取比例保持85%，冷却开关保持关闭，推理模式仍为powerpoint_bps。

peer入口仍为172.18.0.1:18879，当前后端127.0.0.1:18891。切换时旧代有41条连接，保留到自然排空；19:42最后检查已为零，控制器确认零待持久化工作后安全退役旧928代，没有中断其流式请求。新814代当时已有17条WebSocket连接。隧道active、NRestarts=0，两个主机新的直接root SSH登录正常。

最新证据：`.local/error-repair-20260930/peer-verified-114202.json`、`peer-roll-activate-114046.json`、`peer-final-114251.json`。最后一份包含真实18891监听和持久入口目标匹配检查。

后续peer发布命令：

```powershell
python .local/error-repair-20260930/roll_peer.py RELEASE_SHA EXPECTED_ACTIVE_BINARY_SHA
```

新启动的入口进程首次控制写入前，last_update_durable为null。发布工具仅在当前目标完全匹配时，对同一个目标做revision+1的CAS持久化确认，再检查目标文件与确认结果一致；流向和连接不变，不跳过持久性检查。

本次未增加限流等待或更改并发规则。
