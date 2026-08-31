# DECISIONS — 语义决策记录

> 每条可追溯到官方源码。新决策追加在文末；wire 契约冻结清单在本文件末尾。

## 语义决策记录（每条可追溯到官方）

- **Typert Gateway Go 适配决策**：官方 SRC fallback（Function.prototype.toString 提取 JS 方法签名 + @Remote 标记运行时推导描述符）无 Go 对应物（reflect 不暴露参数名），如实受阻——Go 只解析 strict 注册定义，未知 endpoint 走官方空候选路径同文错误；typertRemote binding 标记检查不需要——strict 描述符注册本身就是绑定（Go 无装饰器面）；cancellation 注入位置取 Go 惯例首位（context.Context 第一参数）而非官方 JS 末位追加（signal 是描述符元数据非 wire 参数，位置属载体定义域）；JSON-safety 断言=json.Marshal（循环引用/非有限数/func/chan 天然拒绝）替代官方逐字段遍历；strict codec 校验只断言不变换（无 Zod parse 的变换语义，typert 轮已记录）；业务方法经 reflect MethodByName 解析、结果按 Go 惯例 (result[, error]) 折叠、nil 参数按方法签名补类型化零值。
- **Typert 运行时移植边界决策**：官方 typert 四包中 registry+protocol 落地为 Go 运行时（generator=构建期 TS AST 工具、loader=经 node:module 类型剥离的运行时装载——两者无 Go 对应物，如实受阻不硬凑；toJSONSchema 投影属 Zod 生态随之延后）；TS declaration merging 的 TypertLookupMap/ContextMap（编译期键扩展）→ Go 字符串键运行时表 + any host/identity 边界，类型安全塌缩到注册边界（与既往 any 断言收拢决策同形）；Zod schema 校验 → Go Validate 闭包（无 schema 反射/JSON Schema 生成）；lookup 解析覆写（configure）建模为 provider 之上的可撤销组合层而非原地可变解析器——覆写可先于 provider 注册（声明稳定界不变），dispose 按指针等值精确还原本覆写；Registry 绑定自身 cordis context，贡献注册以 Effect 挂撤——Dispose 后注册即被撤（cordis 语义的自然推论，文档化）。
- **SQLite 持久后端驱动 = modernc.org/sqlite v1.34.5（纯 Go 无 cgo），替代官方 node:sqlite DatabaseSync**（R24）：官方后端绑定 Node 24 内置 node:sqlite，本机无 node；Go 侧 database/sql 纯 Go 驱动承载同一 schema/契约（零工具链要求——mattn 需 gcc 有 Windows 风险，落选）；单连接（MaxOpenConns(1)）等价官方单 DatabaseSync 句柄，BEGIN IMMEDIATE/DEFERRED 记账在同一连接上精确；锁等待由 busy_timeout pragma 承接（官方手工 10ms 重试轮的等价物）；journal_mode 校验对 :memory: 期望 memory 与官方一致。体积代价：modernc.org/sqlite + modernc.org/libc（C 运行时转译）族是本库最重的第三方增量（sqlite 后端较 jsonl 后端多约 26 个依赖包）；`cmd/dsh` 二进制实测约 18.7 MB（Go 空二进制基线约 1.5 MB）——为零 cgo 承诺支付的明示成本，官方 node:sqlite 随 Node 内建零附加。诚实降级：chunk 行装箱（codec.ts）与行压缩（compression.ts）延后——本构建只写 is_packed=0，外来 packed 行读端 fail-loud（提交前缀内=corrupt 硬错误、其后=torn 尾——官方 scanRows 提交前缀规则本身已移植，判定逐字一致）。来源：session-persistence-sqlite schema.ts/store.ts + dsh-go session/persistence/sqlite。
- **attachment 光栅管线 = Go 标准库，替代官方 sharp**：PNG/JPEG/GIF 全解码再编码（EXIF orientation 不施加——带方向的 JPEG 必携元数据故恒以无方向重编码）；WebP 仅头部探针准入（媒体类型/尺寸/动画/透明）不能重编码，超预算 WebP 在规范化步失败而非透码——保留“准入宽、规范化有底”的诚实性，能力缺口如实记录。来源：attachment-local detect/normalize + dsh-go attachment/local。
- **可执行文件解析 = PATH 惰性解析，替代官方打包二进制**：fssearch 不打包 @vscode/ripgrep，从 PATH 惰性解析 rg（一次/进程、rgPath 配置可钉死）——缺二进制在首次调用 SEARCH_FAILED 而非组装失败（与官方 call-boundary 解析同语义）；shell 域 Windows 的 bash 是 WSL stub（官方 tool-bash 亦 win32 禁用），bash 执行器真跑测仅 POSIX 生效，win32 以 pwsh 执行器跑同套生命周期。来源：剩余条目 7/9 诚实记录 + dsh-go fssearch/shelllocal。
- **workflow 脚本执行域 = Go 原生函数域，替代官方 worker-thread plain-JS realm**（R10）：官方第三方 workflow 脚本是 JS 源文本，跑在 worker 线程 JS realm（宿主求值隔离 + 超时可杀）；Go 侧对应物是编译期装配的 Go 脚本函数（`StartRequest.Program`），引擎宿主契约（ScriptAPI 六件、事件词表、配对/settle/fatal 纪律、invariant 观察）与官方逐语义等价，事件 wire 不变。**兼容边界（明示）**：官方 JS 脚本源文本在 Go 域不可执行——`Program` 即 Go 侧脚本形态；官方"meta 校验绝不求值脚本文本"的隔离理由在 Go 侧同样成立（plain JSON meta 与 Go 函数域天然分离）。来源：`packages/workflow/workflow-worker-thread` 宿主契约 + dsh-go `workflow/engine.go`。

- **agent 事件总线类型化访问器（`TypedWaterfall[T,R]`，样板=agent/pre-step）**：Go 无泛型方法，故以值类型句柄（`Events().PreStep().On/Dispatch`）绑定载荷/结果类型，any 断言只存在于类型边界一处（构造保证其成立）；监听器表、作用域准入、first-registered-outermost 次序、"next 恰好一次"契约全部不变——同一张 any 表驱动，raw `OnWaterfall` 仍可用于未迁移事件（同一事件名不得混用两种访问器）。迁移只是消灭消费方的 assert-and-decode 仪式，非行为变更。五个 waterfall 事件已全部接入：`PreStep()`、`Request()`（链上为 `*llm.LlmCallConfig` 指针流——擦除时代 base 值/监听器指针混流的隐性不一致借此显式化并统一）、`RequestError()`、`userquestions.Requests()`、`userapproval.Approvals()`。
- **interaction 两 waterfall 的联合类型 seam 收敛**：user-questions 的 R 联合（`Answer|*Answer|UserQuestionError|*UserQuestionError|error`）收敛为单一具体决策类型 `QuestionDecision{Answer, Err}`——不可识别返回形状的运行时归一分支被编译期排除（原"foreign shape→NoProvider 归一"的 fail-closed 语义上移为类型边界保证；answerer panic 仍由 dispatch 的 recover 含容为错误）。userapproval 的 R=词表字符串保留，但类型经 seam 闭合；**词表值归一保留**（typed 仍可能携带任意字符串值，`outcomes[decided]` 检查照旧归一 Unavailable），panic 含容改为 `containedDispatch`（recover→fail-closed base，语义不变）。事件名、payload、作用域准入、组合次序零变化。
- **委派策略落盘闭合（`sandbox/mode` 接缝接通）**：child-agent 轮预留的"captured override 仅内存"接缝闭合——`permissionpresets.Service.OverrideOf` 实现了 `subagent.SandboxOverrideService`（只读会话显式 `sandbox/mode` 折叠，绝不回退部署默认），`AppendDelegatedPolicyOverrides` 在 approval pin 之外追加 `sandbox/mode` pin（typed 词汇、create 分支、未发布创建窗口内），子日志仅含本构建可折叠的类型——冷恢复从日志重建同款委派策略，不再依赖内存窗口。
- **emit/serial 总线类型化（`TypedEmit[T]`/`TypedSerial[T,V]`）**：fire-and-forget 与串行链同一边界规则——载荷断言收拢到句柄一处，按事件 accessor（agent 包方法 `Created/Status/SessionStart/InboxClaimed/InboxDiscarded/TurnStopping`；subagent 载荷类型对 agent 不可见，`subagent.Starts/Ends(bus)` 包级构造）。生产 11 处 emit + 2 处 serial 监听器全部迁移，raw `OnEmit/OnSerial` 保留给测试与一次性用途（同一事件名不得混用）。serial 的 bail 值类型在生产中从未非 nil（hooks 边界只经 inbox steering），`TurnStopping` 的 V 暂为 any——诚实形态，非留白。**迁移暴露并修复一处潜伏缺陷**：continuation manager 的 accepted-id 排水监听器用共享擦除解码器只匹配 discard 载荷形状，而 claimed 事件携带不同 struct——"claim 或 discard 排水"文档语义中 claim 一半是死路径；分型强制两个载荷类型拆开后修复，回归测试钉住两条边。
- **投影单元泛型化（`projection.Unit[S]` + 显式 changed 门）**：Go 泛型类型+擦除构造器（`Unit[S].Definition()`）替代全 `any` 状态与"同引用=无变化"的隐式门——`Apply` 返回 `(S, bool)`，false 显式透传旧状态（注册表引用快路径逐位保留），true 才发布。any 断言只存在于 `Definition()` 类型边界一处（构造保证成立：单元状态只出自同单元 Init/Apply/DecodeState）。"新分配但未变化的状态谎报变更"这一类静默 bug 被编译器消灭。六个单元（sessionstats/subagent×2/todo/planmode/agentPreset/permissions）全部迁移；`permissions` 的共享 fold 在单元边界以可比较结构体等价计算 changed。客户端 JSON 值逐字不变（agentPreset 的 null/string 由 View 展开保证）。
- **cordis 服务键类型化（`cordis.Service[T]`）**：`DefineService[T](name)` 把服务名绑定到 Go 类型，any 断言收拢到句柄一处——`From(ctx)` 沿祖先链解析（absent/nil ctx/错误类型一律 `ok=false` 降级，不 panic），`Provide` 对称发布。已接线：`agent.ContextService`（"agent"，factory 发布/子代理创建窗口消费）、`webserver.ContextService`（"webServer"）。tools 注册表的 `Get(name, scope)` 是键控工具表不是服务定位器，不在此列。
- **WeakMap→`weak.Pointer` 评估结论：不采用**（如采用需 go directive 1.23→1.24）。调查 37 处生产站点（14 文件）：全部要么带显式 delete/dispose 钩子（projection cells、continuation、controller pending、coordinator live、engine overflow*、transaction byAgent…），要么与宿主同生命周期（meter states、sessionlog folds）。Go 的显式确定性清理**严格优于** JS WeakMap 的 GC 时机不确定——原 TS 痛点在 Go 移植中已被更idiom的机制消除；引入 weak 反而把确定性换成 GC 定时。重审条件：出现全局长寿命注册表以会话对象为键且无淘汰钩子的泄漏面（当前未发现）。
- **会话词汇注册收拢到装配层（②）**：12 个域包的 `init()` 副作用注册（含 panic 路径与"导入即注册"）改为各包显式 `RegisterEvents()`，由 `boot.RegisterVocabulary()`（`Assemble` 第一步）统一调用——静态构建的"插件加载时刻"就是装配，词汇成员资格由这一处显式决定。注册走幂等的 `session.EnsureEventTypes`（重复装配安全）；严格版 `RegisterEventType` 保留给真插件合并语义与冲突测试。词汇门只在持久读路径咨询（persistence coordinator），内存 append 不设门——因此契约是：**一切入口必须经 `boot.Assemble`（或先调 `RegisterVocabulary`）**，越过装配面读日志按 fail-closed 拒绝（拒绝而非腐化，安全方向）。
- **Get 仅沿祖先链**（self → parents）；Waterfall 必须调 `next()`；Effect 在 Dispose 之后注册立即执行 disposer；Dispose 收敛所有失败。来源：cordis-primer + postmortem 0001。
- **patch override 是整字段替换**（`target[key] = value`，无合并）；patch 未命中目标只告警不报错；插入行被索引供同一列表中后续 patch 命中。来源：`vendor/include` `applyEntryPatches`。
- **`!!js` 同时匹配速记 `!!js` 与完整 `tag:yaml.org,2002:js`**：yaml.v3 对未知速记保留字面 tag，与 js-yaml 的解析产出不同，两种形态都要接受。
- **settings 深相等写入不发事件、不增修订**：对齐官方 deep-equality 规则，同时杜绝 save→reload 回环。
- **watcher 在存储锁释放后派发**：持久化 watcher 回写文档需要再拿锁——持锁派发会死锁（已在测试中暴露并修复）。
- **credentials 记录的 scope 是属主插件名**（不是域），载荷按属主格式写，`/` 保证两个键空间文法不相交。
- **session 事件 Data 用 `json.RawMessage`**：词表可扩展且逐字节兼容官方日志；已知类型提供类型化解码器，未知类型拒绝整份日志（fail-closed，加普通事件类型不 bump 格式版本）。
- **workspace 实体 no-op 门 = 引用相等**（对齐官方 `changed === current`）：Go 值语义无法携带引用身份，`errNoChange` 哨兵替代"fn 原样返回输入"；`SetTitle` 恒构造新记录 → 同值也落盘并刷新 `updatedAt`（官方 `{ ...record, title }` 行为），幂等路径（attach 已计入 / detach 缺席 / 移到原位）保持无写入。
- **timecontext 浏览器时区投毒走 `PreStepReject`**（官方沿 waterfall 抛错 → error 终结）：Go 同步 `OnWaterfall` 接缝无错误通道，拒绝是终结语义下最接近的务实选择——终结词汇可观察不同，已记录为适配。
- **planmode `Set` 的 durable append 失败返回 error 给调用方**（官方 append 抛异常传播）：不再吞成 `queued`；pending 选择保留可重试（delete 只在成功后），与官方 `pendingIntents` 生命周期一致。
- **storagedomain 监听器锁外派发**（对齐官方单线程 emit 的同步重入能力）：写路径 mu 内完成 backend+内存提交并登记队列，派发在 mu 释放后按提交序进行；监听器可同步读快照甚至再入队写（嵌套写事件 rides 同一队列，仍保提交序）。Go 适配：并发写者下，派发中读内存可能看到更晚提交的值（官方内联 emit 不会交错）；事件载荷始终是提交时值。
- **TS `??` → Go 映射约定**（全局排查结论）：`??` 仅对 `undefined`/`null` 回退、不捕空串——Go 侧按操作数形态选映射：可选标量用指针/`(value, ok)` 元组（如 permissionpresets 的 `EffectiveSandboxMode`）、map 读取用 ok-check、`string` 一律以 `""` ↔ `undefined`（空串即"未传"）。特例：workspace `Create(path, title)` 的空标题即"未传"→ basename（官方 `title ?? basename`；官方无调用方显式传 `""`，行为无损——R8 决策）；plan 投影 view 的 `running?.wanted ?? wanted` 用指针逐字对齐；未来 exit 工具 port 时 `firstHeading(args.plan) ?? 'Plan'` 须按 `""`→`'Plan'` 映射。
- **subagent 生命周期边经 registry 事件总线派发**（官方 `ctx.events.dispatch('emit', [carrier, name, info])` 自带逐监听遏制+logger 告警）：Go `SubjectEventBus.Emit` 契约已含逐监听遏制，运行时按 parent 的 ScopeKey 作 dispatch carrier（provider removal 无载体→nil 全局）；start/end 以**同一次铸造的 runId** 配对（identity 只建一次），start 同步先发后启终局 goroutine 复刻 Promise 反应时序；`EpochStopReason` 的 aborted-vs-error 优先序：recorded failure 胜过 droppedUnrun 取消——"停掉一个已经失败的孩子不把失败改写成取消"。
- **deepseek 扩展注册表为可选伴件服务**（官方 `ctx.get('deepseekLlmApiExtensions')` 非静态 inject）：Go 把注册表提为 `deepseekLlmApiExtensions` 服务（deepseek-llm-api-extensions 条目 Provide），llm-deepseek 装配经 `PluginDeps.Extensions` 缝在 Apply 期机会式读取（缺席=nil 适配器无扩展——适配器构造时冻结，官方同构）；profile 组合序需把扩展行排在 llm-deepseek 之前（compose 序即官方可用性驱动序的静态化）。
- **session-checkpoint-policy 的 Flusher 适配**：官方 flush 走 `ctx.sessions.flush(session)`；Go `Coordinator.FlushSession` 接 `*session.Session`——catalog 以 id→`session.Store.Get` 适配，查无会话=Flusher 报错即拒绝检查点（保守：阻止适配器分发而非静默放行未持久化前缀）。
- **storage 族装配：routed backend 表 apply 期解析**（官方域插件 `inject(backendServices, …)` 用 `storage.backend.*` 生命周期键保证激活不与 backend 注册竞速）：Go catalog 条目把生命周期键放进 Inject 列表（同一激活门），但 routed 表在 Apply 内从 hub 注册表解析成 `map[string]storagedomain.Backend`——路由名未注册在装载期 fail-loud 而非官方的首次 open `backend-not-found`（激活门已保证名字在场的常规路径行为一致；这是把"竞速不可能"前提显式化的收紧，非语义偏离）。config `root` 必填无回退逐字对齐官方立场（cwd 回退会把单元文件散落进程启动目录）；Dispose 链=unregister→Close（hub 不拥有 backend 生命周期，与官方 disposer 序一致）。
- **spill 族装配：store 可选语义与 owner 解析**（官方 spill-policy `ctx.get('spillStore')` 非静态 inject——spill 存储是可选伴件）：Go catalog 条目同样不把 `spillStore` 放进 Inject，Apply 内 `ctx.Get` nil 容忍（无后端时 Attach 仍挂监听、保存失败/无 store 走 warn+保原文）；`resolveOwner` 经既有 `agentResolverOf`（ScopeKey→agent→`Agent.ID` 即 session id）替代官方 `exec.agent.session.header.id`；`maxInlineBytes` 缺省=不注册（官方 z.number().required() 更严——base bundle 常供 50000，Go 侧宽容缺省以支持不挂策略的 profile，记录偏差）；fssearch 的 `trySaveFormattedResult` 工具臂不在本轮——Go Render 面无 exec/ctx 通道，随展示轮。


## 锁定的 wire 契约（改动 = 破坏第三方插件）

- 路由 kind 词汇表：`exact` / `prefix` / `fallback`；服务键 `webServer`、`settings`。
- llm 消息/内容块/流块 JSON 字段名（camelCase，与官方日志逐字兼容）。
- `SessionEventMap` 事件词表、SessionHeader 字段、SurfaceOp 双形态。
- settings 文档 = 命名空间 → 段 YAML/JSON 映射。


## 后续追加（维护期）

- **2026-08-31 并发派发串行化（jobs）**：官方 JS 单线程保证监听器串行执行；Go 移植引入 `dispatchMu` 串行化 jobs 完成监听与 changed 通知派发——恢复官方语义而非仅修 race。锁序：先 `mu` 后 `dispatchMu`，二者不嵌套。
- **2026-08-31 runHook 接缝原子化（hooks 双桥）**：detached 运行从跟踪 goroutine 调用接缝而测试换装接缝——包级 func 变量改 `atomic.Pointer`。官方单线程下无此问题，属 Go 并发模型适配。
- **2026-08-31 移植基线升级跟踪**：基线从 dsh-v0.1.2-alpha.1 升至 dsh-v0.1.2-alpha.2（上游 HEAD 0a53fb55be）。delta 清单与执行序见 [ROADMAP.md](ROADMAP.md)。ignorable 信封与注册机制立场冲突的决策待移植轮 1 定谳入账。
- **2026-08-31 测试环境约定**：`TEMP`/`TMP` 必须为长路径（8.3 短路径破坏 canonical 化断言，用户级已修正）；`-race` 依赖 WinLibs gcc 16（旧 8.1.0 与 go1.26 race runtime 不兼容）。

## SESSION_FORMAT_VERSION=0 格式演进推演（2026-08-31，A4 落档）

版本 0 = 无兼容承诺；演进原语已就位：persistence 读路径方向感知拒绝
（"upgrade the harness" 携 raw log 位置）、格式拒绝先于结构校验、
SESSION_FORMAT_UNSUPPORTED 不与 CORRUPTION 混淆。真实日志积累前的推演结论：

1. **结构性变更才 bump**（header 形状/信封/core 事件语义/surface 机制——types.ts 注释
   原文立场，alpha.2 未变）；普通事件加型不 bump。
2. **词汇增长的兼容机制 = ignorable 信封标记**（alpha.2 定谳，替代事件名注册——
   上游明确否决注册机制："does not classify omission safety and would make reads
   composition-dependent"）。
3. **dsh-go 立场决策**：EnsureEventTypes 保留为宿主静态装配层的词汇登记（幂等、
   fail-loud 重复），但**持久读路径守卫改以 ignorable 标记为权威**——未识别类型：
   带标记跳过、缺席拒绝（与上游逐字一致）；注册表退为装配期一致性检查面，不再是
   读路径门。此决策随移植轮 1（W1）实施并测试钉住。
4. **bump 演练**：假想 SESSION_FORMAT_VERSION 0→1 时，jsonl 路径走
   SessionFormatUnsupportedError 分支方向感知提示（旧日志被新运行时读=提示升级 harness；
   新日志被旧运行时读=拒绝并附 raw log 位置）；sqlite 路径 user_version 闸门同构。
   两后端已在测试中钉住拒绝词形，推演确认无需新增原语。
## alpha.2 移植轮决策（2026-08-31，轮 1-4）

- **ignorable 信封与注册机制的关系**：持久读路径守卫改以 ignorable 标记为权威
  （未识别类型：带标记跳过、缺席拒绝——与上游逐字一致）；EnsureEventTypes 保留为
  宿主装配层词汇登记（幂等 fail-loud），不再是读路径门。信封解码拒绝
  ignorable:false（wire 类型只有 true；Go 在解码边界执行 TS 类型声明的
  ignorable?: true，比上游 JS 的 seed-path-only 校验更严——已记录）。
- **turnBoundary 单元归属**：agent-loop 注册并拥有 turnBoundary 单元
  （stateVersion 2，折叠四类边界事件）；NewAgentLoop 增必需 projections 参数
  （对齐上游 "require the projection registry"）；hooks 双桥 turn 序号改读投影，
  Apply 增必需 projections 参数（对齐上游 inject 变更），测试经
  	estProjections(t) 注册单元。
- **gateway Remote 失败词表收敛**：17 码 gateway/ 前缀化（wire 破坏性，对齐上游
  remote-error-codes.ts）；WireFailure 补 *GatewayError 分支——修复 alpha.1 Go 侧
  网关错误在 wire 边界塌缩为 internal 的缺口；typed details
  { endpoint, field? } 上 wire（field 缺席不上 wire）；共享码
  	ypert.CodeSessionNotFound = "session/not-found" 落 typert（Remote 层共享词表的家）。
- **atomicwrite 包**：对齐上游 util/atomic-write——win32 瞬态集
  （ERROR_ACCESS_DENIED/SHARING_VIOLATION/LOCK_VIOLATION 对应 EACCES/EBUSY/EPERM）、
  20ms 起 ×2 指数帽 200ms、8 次上限；settings/file（原任意错误 10×50ms 固定——
  语义偏差已收敛）与 storagejson（原无重试）两站点接用。
- **goal 折叠（B3）核实**：r49 的 goal 包落地晚于 alpha.2，fold 已含 goal-source
  rounds 语义——上游的 light-projection 读路径差异记录为 Go 偏好严格折叠的既有
  立场（fail-loud 重放），不另改。
## 轮 6 决策（2026-08-31）

- **plan-mode require-registry**：NewController 增必需 projections 参数（对齐上游
  8645053ca0）；plan 单元由 plan-mode 条目生产注册（此前定义存在但从未注册——
  生产死代码，本轮接通）；Controller 的 active 读经 StateOf 投影（首触折叠历史，
  之后 O(1)）。HasOpenTurn 暂保留事件扫描（turnBoundary 收敛留待下轮，无语义差）。
- **preset live-mount-first 架构性 N/A**：上游场景是 cordis 多运行时 + HMR 下
  "挂载后的文件被改坏不影响 standing composition"；Go 静态单运行时组合在启动时
  读取一次组合，运行期文件损坏本就不影响已挂载预设——上游修复所针对的竞态窗口
  在 Go 架构中不存在，不移植（非降级）。
- **sessiontitle O(1) 投影延后**：Go 读路径每次全量折叠，行为与上游一致，差异仅
  每读代价；与 deque 同类，profiling 证实热点后再移植。