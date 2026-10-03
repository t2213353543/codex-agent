# 环境工具独立 Linux 离线验收

2026-09-25，在独立 `.local/research-20260925/environment-tooling-validation/` 冻结 3 个工具、3 个测试及 3 个依赖，上传到 `/opt/codex-agent-integration/checks/research-20260925/environment-tooling-validation/tooling`。未覆盖旧 exact-account-control 或旧冻结工具，也未修改任何 Go 生产、线上配置或历史 start 脚本。

单个容器 `codex-environment-tooling-validation-20260925`（ID `4f43286653b63862a7b06027824fdc7ed297a3ed36c9ad4b5ad127ca63f27f58`）顺序执行全部三套 unittest，最终正常退出 0、无 OOM。Docker inspect 核实 network=none、2 CPU、内存 1 GiB；未挂载账号凭据。真实 Rust helper 仅访问容器内 synthetic loopback。

| 套件 | 执行 | 失败/错误/跳过 | 耗时 |
|---|---:|---:|---:|
| test_quality_exact_request_probe | 21 | 0 / 0 / 0 | 0.846s |
| test_quality_usage_probe | 21 | 0 / 0 / 0 | 0.664s |
| test_quality_account_control | 5 | 0 / 0 / 0 | 0.001s |
| 合计 | **47** | **0 / 0 / 0** | 1.511s 套件累计 |

真实 Rust→HTTP POST 已执行，验证固定 body/header 字节、reasoning item 隐私摘要、单次派发和 helper 回收；真实 Rust→HTTP GET usage 已执行，验证显式 synthetic application identity、无 Reserve/reset 请求、单次派发和回收。POSIX 文件权限/符号链接、短读写绝对 deadline 等用例无跳过。account-control 只有 stub 回调，不发送模型请求。

运行镜像 ID `sha256:2f1ec799b1a2f1e323acc9a9967ad0aeee9f2836a5d1d3e08867c7df2616ba43`，Python 3.14.7；只读挂载现有 `/opt/codex-agent-integration/checks/audit-20260924/native/work/target/release`。实际 helper SHA `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。

| 冻结文件 | SHA-256 |
|---|---|
| quality_exact_request_probe.py | `1e448cff75e9f02b688bbe665d999e502a658797e5c87b7433e06b624d604749` |
| test_quality_exact_request_probe.py | `e7dcabf344ce3cda0a77052ca3c2e0be93c20b85910b90452e0828d77d939b0f` |
| quality_usage_probe.py | `612589401c34acd6ded40578ffbd057a4a8f0793e335bf728335f875e4be01d1` |
| test_quality_usage_probe.py | `9c03295c9ae7881451f0088f486e89111b66f4bc3ac2078eb873ca800591661f` |
| quality_account_control.py | `0df60369aa96ded047ceaa7da935d6bbbcdb70f34f89ed5645416b8dd68e8b2c` |
| test_quality_account_control.py | `d48d6ade263ffbd01b367a33cc6cd60b04eed054566aaa30f1879e4f7a752d93` |

依赖 SHA、runner SHA、UTC 冻结时间见 `freeze-manifest.json`；容器最终状态及挂载见 `container-inspect.json`；逐套件结果见 `validation.json` 和三个 `.log`。均同时保留在本地独立目录与远端同名目录。本轮一次启动、一次执行，无失败重跑，模型请求数 0。

读取同一已退出容器的状态与日志（不会重跑测试）：

```powershell
python .local/research-20260925/environment-tooling-validation/validate.py status
```

复验冻结源码时可在同等隔离 Linux 环境提供 `CODEX_NATIVE_HTTP_TEST_BIN`，顺序运行 `python3 -m unittest discover -s tooling -p test_quality_exact_request_probe.py -v` 及另两个对应测试文件；完整本轮单容器命令已保存在 `launch.json`。旧 start 因工具 SHA 更新而拒绝属于预期 pin 边界，本轮未改写历史执行入口。
