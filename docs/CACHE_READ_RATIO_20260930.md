# 对外缓存读取比例控制

设置页在“缓存创建按读取返回”下面提供“缓存率控制”。默认关闭，比例为0；可保存0–100的整数，关闭时仍保留预设比例。

开启后对外 cached_tokens = floor(input_tokens × percent / 100)。先执行可选的“缓存创建按读取返回”，再由比例覆盖对外读取量。输入、输出、总Token不变；本系统真实读取/创建量、日志与计费聚合继续使用上游原始用量。未知或非法input计数不补造总输入，正文、加密历史和工具参数的原始字节不变。

字段为cache_read_ratio_enabled、cache_read_ratio_percent，沿用/api/settings的POST + settings_version部分更新和409冲突契约。旧配置默认关闭；设置持久化、跨进程读取、边界校验均已测试。

已部署release db50b10969d541a2046ff9c53ed75f059bb606131871b2894f7fe7ba3bfd6d12，二进制9281578adb95f3857a64e0d08fd383dca500e3959a5e8c0021598102ca02d24c。前端构建与浏览器交互（0/100、小数/非法值、失败回滚、409、移动布局），JSON/SSE/WebSocket/compact/structured输出和原始统计隔离，真实PG部分更新/持久化均通过。上线只读检查确认默认关闭且settings_version仍为37。

缓存率是对外用量的展示/返回策略，不改变模型实际缓存，也不改变Redis续聊缓存1小时/512MiB的保留限制。

用户随后明确不增加限流等待；本次release没有包含短冷却等待补丁。
