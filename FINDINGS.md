# FINDINGS — 审查发现台账

> 发现 → 修复/入账 强制闭环。R 系列来自验证 Agent 对照 `_dsh-official` 的语义审查；
> F 系列来自维护期（race 复检/环境甄别）。条目定谳后只追加不改写。

## R 系列（对照官方语义审查，R1-R11）

| # | 严重度 | 位置 | 发现 | 状态 |
|---|---|---|---|---|
| R1 | 中 | workspace/entity.go | 官方引用相等判 no-op，Go 值相等误吞 `SetTitle` 同值写 | ✅ 已修（`errNoChange` 哨兵 + 测试） |
| R2 | 低 | workspace/entity.go:198 | 错误链丢失 cause | ✅ 已修（`%w` 保链） |
| R3 | 低 | timecontext Register | 时区投毒拒绝路径终结词汇可观察分歧 | ✅ 已入账（PreStepReject 接缝约束） |
| R4 | 低 | planmode Set | append 失败被吞成 queued | ✅ 已修（返回 error，pending 保留可重试） |
| R5 | 中 | planmode SectionText | `pending?.active ?? fold` 语义误读，pending 窗口 section 多显示 | ✅ 已修 + 测试 |
| R6 | 中高 | storagedomain emitLocked | 持锁派发监听器 = Go 死锁面（JS 单线程天然安全） | ✅ 已修（锁内提交入队、锁外按序派发 + 重入测试） |
| R7 | 低 | commands ImageAdmitter | admission 错误分类未保两分支 | 开放（attachment 轮接线时对齐） |
| R8 | 低 | workspace Create | `??` 不捕空串被误读为 `== ""`（TS `??` 语义系统性误读第二例） | ✅ 已入账（全局 `??` 映射约定，见 DECISIONS.md） |
| R9 | 低中 | subagent continuation-manager | ListSnapshots 失败静默跳过 vs 官方抛错 | 开放（低优先） |
| R10 | 低中 | workflow engine | 引擎已交付但 README 决策记录缺失 | ✅ 已入账（Go 原生脚本域决策） |
| R11 | 低中 | hookprotocol | 竞态根因（重构轮 2 修复族） | ✅ 已修 |

## F 系列（维护期 2026-08-31，全量 `-race` 首轮 + 环境甄别）

| # | 位置 | 发现 | 状态 |
|---|---|---|---|
| F1 | jobs/local.go | Get/Read/Kill 锁外读写 job 字段 + 监听器并发派发（官方 JS 串行语义丢失）——52 警告中 38 处的主窝点 | ✅ 已修（全程持锁 + `dispatchMu` 串行派发，恢复官方语义） |
| F2 | jobs 测试 | `time.Sleep` 凑 happens-before 反模式 | ✅ 已修（channel 确定性断言） |
| F3 | settings/file | `interval` 无锁读写（watch goroutine vs SetPollInterval） | ✅ 已修（`f.mu` 守护） |
| F4 | settings/file 测试 | 计数器裸读写 | ✅ 已修（`atomic.Int32`） |
| F5 | toolweb 测试 | scriptedSearch 并发 append | ✅ 已修（mutex） |
| F6 | identity 测试 | busy-wait 无锁自旋读共享切片 | ✅ 已修（WaitGroup + mutex） |
| F7 | hooks 双桥 | `runHook` 接缝无同步换装 + observed 无锁追加 | ✅ 已修（`atomic.Pointer` 接缝 + `observedLog` 带锁类型） |
| F8 | fssearch 测试 | `TestSearchToolsRealRipgrep` 因"无 rg 即 skip"从未运行——首次执行暴露 2 个潜伏 bug：`.git` 夹具目录未创建、harness 缺 subprocess 装配 | ✅ 已修（测试历史首次全绿） |
| F9 | 环境 | 本机 TEMP 8.3 短路径（`HZ0704~1`）与 canonical 化冲突 → 6 包预存失败 | ✅ 已修（用户级 TEMP/TMP 改长路径；DECISIONS.md 入账） |
| F10 | 工具链 | gcc 8.1.0 与 go1.26 race runtime 不兼容（0xc0000139） | ✅ 已修（WinLibs gcc 16.1.0；README 构建节已更正） |
| F11 | jobs/local.go Read | 修复轮引入的回归（terminal+readOutput 分支漏调 producer 读） | ✅ 已修（忠实原语义重写） |
| R7/R9 | — | 随 attachment 轮 / 低优先处理 | 开放 |
