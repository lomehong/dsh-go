# alpha.3 对齐简报（dsh-v0.1.2-alpha.2 → dsh-v0.1.2-alpha.3）

> 产出：PC-SZ-375 / omp-local-pc（红队/对齐通道）。依据：本地 clone E:\code\nodejs\deepseek-harness @ dd6322d（== tag dsh-v0.1.2-alpha.3）。
> 基线：0a53fb55be（alpha.2，dsh-go 当前跟踪基线）。

## ① Delta 概览

- **117 commits**（含 merge），**1034 files，+11034 / −11047**（增删平衡：重构/测试/文档为主体）
- 热点分布：client ×148、session ×130、apps/cli ×54、subagent ×39、snapshots ×29、api ×22
- **W 系列（wire 破坏性变更）：0 项** —— core/session/src/types、sdk/protocol src、llm/src/types 全程零触碰（仅 protocol package.json 版本行）。dsh-go 的 wire 对齐义务为**空**。

## ② B 系列：运行时行为变更（dsh-go 移植评估清单）

| # | 上游提交 | 语义 | dsh-go 影响面 | 范围 |
|---|---|---|---|---|
| B1 | ceadd90e71（+前驱 8322f804cb/6f0daff1dd，revert a2437db180） | session-projection **identity-gated change feed**：变更馈送在引用同一性之上增加 raw view 输出对比门，Object.is 相同则静默；单位可把工作字段藏在 identity 稳定的投影后 | `session/projection`（变更馈送触发规则） | **core，需移植** |
| B2 | 49bf26a794 | gateway stream-server **tolerate stalled hosts**（19 行 + 测试） | `gateway/gatewaystream`（r61/62 刚移植面） | **core，需移植** |
| B3 | 2c1cc3e778 | closing-turn wake tracking 收窄 | `agentloop` | **core，需移植** |
| B4 | ba810b3539 | subagent 图片 follow-up 准入加固 | `agentloop` + `subagent` | **core，需移植** |
| B5 | 7c38fd8102 | steer/follow-up 图片可靠送达 | `attachment`（core 部分；session-controller/ui 侧超范围） | 半 core |
| B6 | 21d2d9395d | prompt admission 与 echo ownership 对齐 | `attachment` + `llm`（core 部分） | 半 core |
| B7 | 7222e17dc0 + f3c69c2c3f | read_image 接受无扩展名附件路径；image sniffing 收敛 tool-local | `toolfs/readimage` + `attachment` | **core，需移植** |
| B8 | 53f5418a72 | steer 提交回显位置稳定 | session-controller/ui 侧为主 |  mostly 超范围 |

## ③ 分歧决策点（需 Owner 拍板，非降级）

**官方解绑 SQLite 持久后端**（4553c9d957，`!` 破坏级，279 文件）：
- 官方动作 = 装配摘除 + 依赖摘除（agent-team/llm-retry/session-title 各 −1 依赖）+ 架构文档改写；`storage-sqlite` 包本体**保留**（schema.ts 小改）
- dsh-go 现状：storagesqlite + session-persistence-sqlite 为重投入资产（modernc 纯 Go，R24 决策）→ **超越官方**
- 选项：**A. 保留为文档化扩展**（DECISIONS 入账，注明官方 alpha.3 已解绑；推荐——资产有价值且无 wire 影响）/ B. 跟随解绑求 parity

## ④ 超范围（TS/client 侧，dsh-go 不跟）

client/web/cli 主体工作：turn rail + session-turn-outline 投影单元（挂 web bundle）、session-controller loadThrough 深历史翻页、ui-chat/ui-primitives perf、examples/agent-spine-demo 删除、CI/Windows 测试大批。注：turn-outline / loadThrough 若未来进 SDK wire 契约再评估纳入。

## ⑤ 建议执行序

1. B1 projection change-feed（语义密度最高）
2. B2 gatewaystream stalled hosts
3. B3 agentloop wake 收窄
4. B4/B5 图片链（agentloop+subagent+attachment）
5. B6 prompt admission/echo
6. B7 toolfs read_image
7. SQLite 分歧决策入 DECISIONS
- 执行方：dsh-go-porter（待其深度工作收工；重启前**先跑一键安装脚本**再重启——SOP）
- 验证方：omp-local-pc（每项移植后对照上游提交独立复验）
