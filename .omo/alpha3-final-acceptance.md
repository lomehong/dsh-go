# alpha.3 对齐总验收报告

> 验收方：PC-SZ-375 / omp-local-pc（红队/对齐验证通道）。实施方：dsh-go-porter。
> 范围：dsh-v0.1.2-alpha.2（0a53fb55be）→ dsh-v0.1.2-alpha.3（dd6322d，117 commits / 1034 files / +11034 −11047）。
> 对齐产出：r67–r76（HEAD 1453ec8）。

## 一、总判定

**alpha.3 对齐验收通过。** W 系列（wire 破坏性变更）为零——session types / sdk protocol src / llm types 在上游 delta 中零触碰；B 系列运行时行为变更 7 项全部处置完毕（5 移植验收 + 2 考古无操作）；附带交付 3 项（toolsjobs 加固、`dsh web` 产品面、SQLite 决策入账）。全量门禁多环境多遍稳定（113 包 ok 0 FAIL）。

## 二、逐项验收台账

| 项 | 上游依据 | 交付 | 验证方法 | 判定 |
|---|---|---|---|---|
| r67 `dsh web` serve 面 | 产品面缺口（红队重要-9） | host/webhost 新包 + cmd 扩展 | 全量 113 ok + 独立 smoke | ✅（timer 导入失败另案，见四） |
| r68 SQLite 决策 A | Owner 拍板 | DECISIONS 入账 + 回退条件 | 账实核验 | ✅ |
| r69 B1 projection identity-gated change feed | ceadd90e71+8322f804cb+6f0daff1dd | 双槽缓冲+基线推进+sameView | **960 场景穷举镜像上游生成器** + 实现逐点 + 全量 | ✅ |
| r70 B2 gateway 心跳 + stalled hosts | alpha.2 基线缺失层 + 49bf26a794 | 单 sweeper + missed 计数 + MAX=2 + 延迟复核 | 上游逐行对账 + 交底五条全落位 | ✅（验收中锁纪律两处修正） |
| r71 B2 测试加固 | 红队打回（负载 flake） | SetFinalizeScheduler 确定性 seam | seam 纯注入亲验 + 5 连全量绿跨两环境 | ✅ |
| r72 B4 subagent 图片准入 | ba810b3539 | contentHasImage + assertImageCapable | 逐字文案/错误码 + 「无 await 间隙」主张专项验证（ResolveModelInfo 纯内存） | ✅ |
| r73 toolsjobs 加固 | 红队裁定 | notify channel + 事件驱动 await | diff test-only + 前科测试 -count=10 + 双遍全量 | ✅ |
| r74 B6 prompt admission/echo | 21d2d9395d | AdmitPromptContent 导出（对齐上游结构） | 行为等价（sdk/server 零触碰）+ 形状同构 + 依赖方向 | ✅ |
| r75 B7 read_image 无扩展名 | 7222e17dc0 | sniffImageMediaType 四签名 + 三态 | 签名逐字节 + 逐字文案×2 + 门双分支 + 顺序保证 | ✅ |
| r76 disabled 表达式受限求值器 | 红队收敛缺口（eval.go 零接线） | RegisterPlatformEvaluator + Assemble 接线 | 双谓词精确匹配 + **官方树 18 处 !!js 全部为所支持两形态（覆盖度完备）** + fail-loud 诚实边界 | ✅ |
| B3 wake tracking 收窄 | 2c1cc3e778 | **无操作**（merge-base + content 考古：收窄未存活到 alpha.3 tag） | git 考古三方会诊 | ✅（观察项挂 ROADMAP：alpha.4 重开） |
| B5 steer/followup 图片 | 7c38fd8102 | **无操作**（五断言 git 实证：Go 分层已覆盖存活部分） | dd6322d 内容存活分析 + Go 调用面核验 | ✅ |

## 三、稳定性证据

- 全量门禁（默认环境短 TEMP）：r69–r76 期间累计 **10+ 遍全量绿（113 包 ok 0 FAIL）跨两套环境**（红队环境 + porter 默认环境）。
- 负载 flake 零容忍协议：B2 DelayedPong 负载 flake 被双遍跑协议抓获 → 确定性调度器加固（r71）→ 5 连绿。toolsjobs 前科 flake 同批确定性收敛（r73）+ -count=10 压测。
- -race：gatewaystream 五样本（porter 侧 WinLibs gcc 16.1.0）。

## 四、遗留与观察项（非阻塞）

1. **B3 观察项**：上游 master 存活 2c1cc3e778 收窄，预计随 alpha.4 发布——基线升级轮重开 B3。
2. **F8 残余**：gatewaystream 心跳使该路由静默对端可被收割；host/webserver 注册表层的读上限加固不在本轮范围，保持观察。
3. **yuyi Bug ③⑥**：维护方（omp-assist）在档，排期时知会复验。
4. **沙箱原生执法**（Windows ACL restricted-token）：既有延期项，独立安全验证轮。
5. timer/hmr 处置行接线：web profile 全组合轮（ROADMAP 在案）。

## 五、对抗过程纪要（方法论资产）

- 红队两次断言被 porter 反杀（Serial/Waterfall panic 契约、disabled 零强制），porter 三次断言被红队证伪（63766e9 血缘、C1 大部分误报、B3 机制）——**全部经实证裁决，无 survivors-by-default**。
- 双失误根因入库：检索范围管理（cordis/ 泛化全仓）、被打断验证必须补跑、血缘主张一律 merge-base、content-survival 断言必须 git show 到 tag。
- 流程资产：双遍全量协议、确定性调度器 seam 模式（上游 setImmediate 的 Go 等价）、交付附实测统计口径。

—— alpha.3 对齐正式收官。下一次上游基线（alpha.4）升级时，本通道按同一协议重启。
