# B 组 0.4.13 发布复核

2026-09-28 23:41（Asia/Shanghai）：主会话已部署0.4.13，B只读交叉核对通过。release `dd343bda80dbafd2fe593b2ba4dbde443d6c1ffdcb6b69b57799e88e0c76de35`；PID1422598；binary `948c483bcabc7fb130501a7f22bcd43dc3f448840c64928f81ab2fa209d42479`。

## 已核证据

- Go4468 PASS/457环境SKIP；race OAuth12、bound owner2与20协作项实际PASS；PG13含OAuth实际attempt roundtrip真PASS。未把本地skip当PG执行。
- 首live保留exit1：apply_patch最后真实429中断，前面确曾调用patch，原Python检查未记录。补轮只执行该失败例与遗漏三项，同686执行源码与testbinary57d93e…，补轮exit0。该次apply_patch成功可由fixture条件推出文件检查成功，不追溯改写首轮结论。
- 新组合门禁保留原失败，必需缺项、SKIP、错源码/binary/exit和额外失败均拒绝。人工重复同名终态覆盖问题未出现在实际两份日志；已做私有最小加固与11项本地解析验证，供下一轮采用。
- CSV v4为77列，旧75列名称/顺序和值构造未变，仅尾加final code/source；schema2的真实HTTP位于attempts JSON。typed导出null归省略、显式0保留；不声称原null逐字保持。
- 不可变包91产物/32证据、tar92文件完整校验通过。fullsource2eba78f6…与封包时源码一致。685 backend scope项加1既有embedded JSON组成686执行源码，无缺项。
- 主会话15:39:56Z现场验收：SSE/JSON各一次生成均completed/schema2/逻辑与真实HTTP200；非法参数local400为零派发；三条精确CSVv4导出通过。settings27、账号配置及default冷却false保持，六项迁移清单未变。

## 边界与后续

本报告不把组合补证称为单轮全绿，不把smoke称作自然流量改善，也不把真实live没有发生的OAuth refresh写成已在线触发。精确usage与local错误谓词由已核SHA的主会话postdeploy脚本执行，B没有另发请求或重取客户数据。

既有自然未声明工具问题由C组私有候选继续推进；B图片patch矩阵工具保持READY_PRIVATE_NOT_RUN，尚无已验证无损绕过结论。本轮发布未混入这些候选。

## 回执与复跑

全部B产物位于 `.local/cross-session-20260928/01a0e7ed/`：

- `v0413-release-audit/README.md`、`composed-live-audit.json`、`package-dd343bda-audit.json`、`deployment-audit.json`。
- `v0413-export-audit/REPORT.md`、`v0413-live-failure-review/REPORT.md`。
- `v0413-release-audit/recovery-review/README.md`、`hardening-candidate/README.md`。

仅读现有部署回执的复跑命令（不发HTTP/SSH/SQL）：

```powershell
python .local/cross-session-20260928/01a0e7ed/v0413-release-audit/check_deployment.py
```
