# ROADMAP — 路线与移植计划

> 剩余面权威清单、依赖序地图、alpha.2 移植路线。

## 后续路线（按依赖序）

1. ~~`tools` 注册表与执行管线~~（已完成；PTC `run_code` 传输 + presentation 渲染器随 SDK/PTC 轮补）
1. ~~`systemprompt`~~（已完成；cordis 接缝——注册经 `agent.ctx` 派生 scope——随 agent 轮补）
1. ~~`agent`（registry/inbox/consumed-work/dispatch/model-selection）~~（已完成；typert 查找注册随 typert 轮补）
2. ~~`agent-loop`：turn/step 驱动机~~（核心完成：驱动、步进、请求记账、工具调度、runtime-context、服务层/工厂；settings 节安装接缝随 settings 组装轮补）
2. ~~`interaction` 审批/提问/ask-user 接缝~~（已完成：userapproval + userquestions + toolaskuser）、~~`interaction/commands`~~（已完成：注册表/ParseCommand/lifecycle 事件/图片准入门/change 通知；UI dispatch 适配器随 webserver 轮）、~~`interaction/permission-presets`~~（已完成：服务/投影/`/permission` 命令定义；settings section 与 `session/created` 钩子随装配轮）、~~`plan` 模式 exit 工具与 `/plan` 命令~~（commands 注册表已就位——随 user-questions 组装轮接线）；permission-presets 完成
2. ~~`subagent` 基础层+运行时+组装层~~（已完成：depth/descriptor/assistant-output/run-settlement/activation-setup-registry/SubagentRuntime 注册表+Start 能力门+生命周期 start-end 配对/EpochStopReason/ActivationObserver/child-agent 组装全量（深度预算/元数据/options 解析/组装合成/委派策略）/continuation 类型面+ContinuationManager 接缝+manager 核心切片（ChildLock/stateOf/授权/中断/closing scopes）；manager 本体（materialize/create-resume/submit/watchSettlement/drain）、list-children/ACP provider 随后续轮补）
2. ~~`workflow` 接缝层 + 脚本执行引擎~~（已完成：词汇/typed fatal errors/事件词表/EventSink/meta 校验/engine（Go 原生脚本域替代 worker-thread JS realm——决策见"语义决策记录"，官方 JS 脚本源文本在 Go 域不执行）+ invariant 观察器）
2. ~~`todo`~~（已完成：todo_write 工具 + todos 投影单元；client 呈现面随 SDK/web 轮补）
2. ~~`homepaths`/`identity`/`webhook` 规则运行时~~（已完成：home 解析 + 匿名 id + fire-and-forget 规则运行时；webhook 的 SessionCreator 服务组合已落地——`webhook/session.go` 逐字移植官方 session.ts 全事务 + catalog `dsh-webhook` 行 Inject 六服务，GitHub 适配器随需要时补）
2. ~~`compaction` 接缝层~~（已完成：词汇/事件/checkpoint/tool-pairing）、~~`token-meter` 计量核心 + O(1) 投影单元~~（已完成：估计器/表面折叠/路由定价/usage 锚定测量；**投影面已落地**——FoldSurfaceTokens 影子价 O(1) 折叠（compaction/summary·prune 武装 claim、相邻 replace 消费、无 claim 中性、错位 fail-loud）+ tokenUsage/contextPressure/contextBreakdown 三个投影单元（usage 单槽替换样本+retry-started 关槽、pressure 独立 last-wins 槽+projectedTokens=max(0,采样+表面位移)、breakdown 信封 last-wins+消息走同一折叠；状态固定少数几个数，检查点 O(1)）+ **turn-usage 折叠**（attempt 状态机 idle/open/finishClosed/settled、error/aborted finish 关闭、llm/retry 结算+retry-started 同步重开、聚合可选桶全在才在、路由首次出现序去重、任一缺失 fail-closed 返回 nil）；~~`compaction-basic` provider~~（已完成：config/summarizer/region 事务/engine 四触发器；compaction-pruner（tool-result-pruner 模型无关修剪）与 `/compact` 命令接线随后轮）
2. ~~`workspace`~~（实体层 + registry 引导/create-delete 事务全部完成；webhook SessionCreator 事务句柄已就位）、~~`context` 插件 time-context~~（已完成）、~~`plan` 模式核心~~（已完成：折叠/Set 四态/pre-step/plan:policy 节/plan 投影；exit 工具与 `/plan` 命令随 user-questions/commands 轮）、~~`storage-domain` 域运行时~~（已完成：规格/域运行时/facility/MemoryUnit）、~~`storage-json` 文件后端~~（已完成：atomic 发布/format 校验/single+per-record 单元/legacy 引导/backend）、~~storage hub~~（已完成：backend 注册表/hub 表单/服务键；装配轮接入 loader）；`context` 其余插件（tmux/git）、~~storage-sqlite~~（已完成：modernc 纯 Go 驱动 SQLite 持久后端+catalog 条目）
3. ~~`llm` 运行时 + DeepSeek provider~~（已完成；llm-deepseek 插件装配层——settings section 接线、credential 解析钩子、retryPolicy 变更时 Replace 重注册——随 settings/credentials 组装轮补）
4. ~~`tools` + `system-prompt` + `agent` + `agent-loop`（request/header 记录、turn/step 括号、中断收尾、waterfall 请求改写）~~（已完成）
5. ~~`interaction`（approval/ask-user）、`subagent`/`workflow`、`workspace`、`identity`、`compaction`（surface replace 消费者）、`context` 插件（time-context、tmux）、`webhook` 规则运行时~~（已完成；tmuxcontext 为完整移植：Query/Render/ValidateConfig）
6. ~~`sdk` JSON-RPC 服务器 + `boot/app-boot`（profile 装配、patchReload、fail-loud）~~（已完成：sdk/server+client+protocol；boot/profile.go 396 行含 ProfileTemplate/InitProfile/patchReload 全语义）；~~`settings`/`settings/file`/`credentials`/`llm-deepseek`~~（包已交付：Store/PathOp/file 后端/Provider 接缝/deepseek 插件+credentials 解析）

**剩余（按依赖序，2026-08-29 账实对齐后）**：
1. **插件目录与顶层组合（关键路径——目录与顶层组合均已落地，余量在插件覆盖）**：`boot/catalog.go` 目录基建（`NewCatalog(deps)` 官方 npm 说明符→Go 组合 spec，缺名 fail-loud）+ `boot/appboot.go` 顶层组合（`AssembleProfile`：LoadProfile 层序→ComposeEntries→Assemble，`App.Root()` 服务访问）+ **`cmd/dsh` 可运行入口**（-profile/-home/-anchor/-list，实跑验证：模板 profile 自动初始化→bundle 解析→patch 组合→catalog mount→服务表）。已接线 **70/86**（r48 整理后程序化复核终值；批次 1-2-3：tools/commands/settings-file/credentials-local/web/session/session-projection/agent/llm/llm-deepseek（deepseek.Apply 装配层）/session-persistence-jsonl（Coordinator+storeadapter.go 接缝）/session-persistence-sqlite（modernc 纯 Go 驱动 SQLite 后端全钩子+Dispose effect；jsonl 条目同轮补 Dispose 注册）/typert-registry（运行时 registry——schema/包反射/local+Remote 调用定义/lookups/Context 适配器五存储；session+agent 条目 Inject typert 注册官方 lookup（session: Session↔sessionId、agent: Agent↔agentId）与 agent Host Context 适配器）/user-questions/user-approval/permission-presets（服务+settings section+session/created 钩子全量——`permission` settings 区驱动新会话 defaultPreset（Scope.Get 解析 defaults→base→user 即官方 setSource）、OnCreated 可否决钩子逐会话 PinInitialPermission、组合期对存量会话回填；Inject sessions 为声明依赖；组装测：ProviderPush 切换用户区→新会话即随新值）/system-prompt/agent-loop/**subagent（ManagerExt 生产装配**）/**spawn+fork-in-process provider**（`subagent/inprocess.go` 一次性子代理全生命周期驱动；outputSchema 能力位如实 false）/skill+tool-skill+tool-todo+jobs-local+tool-jobs+plan-mode（/plan+exit 工具双注册）+repeat-tool-reminder（Effect 挂双 detach）/**token-meter**（单例重放感知计量器，图片路由定价缝缺省 nil 走估算；Inject projections 注册 tokenUsage/contextPressure/contextBreakdown 三个 O(1) 投影单元——units 见条目"未移植面"上方的 compaction/token-meter 行）/**compaction-tool-result-pruner**（`toolresultpruner` 包：模型无关 head/marker/tail 码点预算剪枝——只处理 content[0] 内嵌 content、标记恰好一次、富块保序、replacement 必须更小且入阈 fail-loud；PruneSession 快照当前 surface 一次防自食、compaction/prune 影子价事件与替换相邻落地、替换保 message id 只换内嵌 content；配置 DecodeConfig 未知键拒绝 + head+marker+tail≤threshold fail-loud；组合纪律：须挂载在 compaction-basic 条目之前（cordis apply 序，与官方 README 一致），缺位即引擎无剪枝独立可用；engine 级集成测：压剪后清压→跳过摘要）/**compaction-basic**（Engine：摘要走 llm runtime、容量走同 runtime 的 model info、落盘走 Coordinator.FlushSession，配置组合期 fail-loud）/**command-compact**（/compact——compactStarter 契约改为携带 Invocation：引擎 CompactNow 的 MaintenanceAgent 由 invocation.Agent 经 maintenanceOwner 适配器绑定，driver.RunMaintenance 承载保留轮）/**tool-subagent-control**（`subagentcontrol` 包：send_message/interrupt_agent 走 runtime Followup/Interrupt（authority=ancestor），list_agents 走 ListChildren/ListDescendants 的 continuable 投影折叠——冷读 queryEngine 经 Coordinator.Load+NewRestored+投影切面，status 三态 running/open-turn 切面、idle/驻留、ready/仅持久化；一次性子代不进发现面）/**shell 域五件**（shell-env/bash-local/pwsh-local/tool-bash/tool-pwsh，见条目 8/9/10）/**storage 族三条**（storage=hub 服务；storage-json=backend `json` 注册进 hub + `storage.backend.json` 生命周期键 + Dispose 链 unregister→Close，config root 必填无回退；storage-domain=domain 形态挂载 + routed backend 表 apply 期解析 + `storageDomain` 服务——组装测：hub→backend→domain put/get 往返→shutdown 卸载）/**spill 族两条**（spill-local=Provide `spillStore` + 构造期启动清扫、cleanupPeriodDays 0=禁清扫；spill-policy=Inject tools/agents、maxInlineBytes 配置、opportunistic spillStore 缺席容忍、resolveOwner 经 agentResolverOf ScopeKey→agent→session id——组装测含 spill SaveText 落盘）/**policy+skill+extensions 批七条**（timeout-policy=`guard.AttachTimeoutPolicy` 挂工具管线；agent-instructions=maxBytes 缺省 65536、DSHHome=profile home；skill-filesystem=ResolveConfig 默认齐备+`RegisterProviderIn` create 回调接 `control.Invalidate`；skill-badge=嵌入资产物化到 `home/skill-badge`；session-checkpoint-policy=Inject 五服务、Flusher=id→live 会话→`Coordinator.FlushSession` 适配（查无会话=拒绝检查点阻止分发）；deepseek-llm-api-extensions=Provide 扩展注册表、llm-deepseek 经新 `PluginDeps.Extensions` 缝机会式消费（官方 ctx.get 同形）；session-log-deepseek=enabled 缺省 false 不注册；subprocess 条目更名 dsh-subprocess-local 对齐官方 bundle 行名）。/**toolsubagent 批**（两 bundle 行同包双 provider：catalog 条目 `buildDelegationTool(provider,toolName)`——spawn 行缺省 continuable、fork 行 one-shot/subagent_fork；语义决策：provider present-check 在 Register 期 fail loud——官方靠 provider registry listener 晚挂载，Go catalog 静态序已保证 provider 行先行，故收直为装配期校验；jobs 由 Inject 改机会式 Get——官方 ctx.get 同形，缺 jobs 的 profile 仍可前台/continuable；in-process spawn/fork provider 补 `PrepareContinuable` continuable 面（官方两 provider 均实现：spawn 空 spec、fork completedTurnPrefix 捕获一次入子转录）——此前 Go 侧缺该面，continuable gate 会 fail loud；model-selection 子面 r30 补齐：Config.modelSelectionSettings 缺省 false（官方同路径），策略/合并/门/preflight/日志事件/settings section/list_subagent_models 全量落地；Go 偏差如实记录——工具 schema 是注册期静态的，官方 per-agent 安装改为例：参数常驻（enabled 时）、策略在 execute 期活解析（会话事件→设置采样 append-once），无 preset 父链回退；provider agentRouteDefaults 面未移植（choiceDescription 恒用父继承变体）；官方 per-agent 晚挂载的 provider present-check 收直为 Register 期校验）。/**toolsubagentreport 批**（r31）：报告工具走 activation setup registry 贡献面（boot 的 ManagerExt.Setup 缝已在）——贡献失败 panic 由 registry 回滚（官方 throw 同形）、注册器撤销不可失败故 disposer 不聚合错误（官方 AggregateError 路径无 Go 对应需求）、子作用域经 agent 上下文服务派生 Scope 后 `RegisterIn`/`Section(scope,…)`；tool-subagent-control 行此前已对齐无需重做）。/**projectioncache 批**（r32）：缓存域按官方 spec 逐字段（名/版本/布局/校验），Store 适配域表；Go 合成把 sessions flush 面接到 persistence coordinator（官方 sessions 服务自带）——sessionsFlushView 双面适配；Open 后的失败路径显式 domain.Close 兜底；基线读作缺席而非报错（缓存语义：代价是重放，不是错值）。待续：其余 17 项——r37 已盘点依赖序（见"剩余面依赖序地图"），另 timer/hmr 两行已处置为上游同态不需要。**session-title/workspace/agent-presets/webhook 批（2026-08-30）**：dsh-session-title 行在合并中被远程 r40 完整版接管（含 Dispose effect）/dsh-workspace（Inject persistence+sessions+storageDomain，双适配器桥接 Coordinator.List 与 live store 头）/dsh-agent-presets（无 Inject、settings 可选注入同官方、default override/clear、standing 失效互接）/dsh-webhook（Inject agents/agentDefaultModel/agentPresets/permissionPresets/sessionTitle/workspace 官方清单逐字，SessionCreator 闭包组合 `webhook.CreateWebhookSession`）；三行均为 overlay 行（workspace/agent-presets 在 web-app patch、webhook 在 CLI overlay），计入后 69/86，另 catalog 实测行数 73（含委派子行）。组装测：十三条目 Assemble 后四服务在位 + title rename/空白拒绝 + workspace create/list + roster resolve/standing key/ghost 拒绝 + webhook register/dispose + shutdown。**整理合并批（2026-08-30，r45-r48）**：本地线并入三行——`dsh-session-query-sqlite`（FTS5 派生读模型 store 半体+懒打开）、`dsh-tool-web`（模型面 web_search/web_fetch）、`dsh-web-search-deepseek`（检索 provider，自此 web 族全清）；本地线并行的 webs/websfetchhttp 重复实现弃置采远程超集；toolsjobs 完成投递测试竞态修复（锁内快照访问器）

### 剩余面权威清单（收尾态：r39 程序化审计逐条 vs catalog 实名，r48 深盘定谳）

- **wired 70/86**（r39 起以程序化比对为准；合并后终值——远程线：三条 overlay 行 + `dsh-web` 缝 + `dsh-web-fetch-http`；本地线并入：`dsh-tool-web`、`dsh-web-search-deepseek`、`dsh-session-query-sqlite`）。
- **处置 7 行（不移植，实证）**：`cordis-plugin-hmr`（官方 base yml disabled:true）；`cordis-plugin-timer`（唯一消费者 patchReload live 监视官方走 launcher watch-only 兜底）；`dsh-typert-loader`（loader 动态挂载面的 manifest 自动注册——Go 无动态加载，registry 行静态组装覆盖同能力）；`dsh-plugin-package-inventory-deepseek`（注入 loader 服务盘点 npm 插件包——同因 N-A）；`dsh-workflow-worker-thread`（Node worker 执行模型 JS——Go 引擎直执行编译态 Script，engine.go 架构注释即此义）；`dsh-tool-workflow`（模型面 JS 脚本工具——Go 无 JS 运行时，与 PTC run_code 同因受阻；Go workflow 引擎保留为 Go 侧编排库面）；`dsh-llm-pi-ai`（r48 深盘定谳：外部 `@earendil-works/pi-ai` SDK 的多 provider 适配器——官方 README 自述"部署无其他 provider 需求时选 dsh-llm-deepseek 直连"，Go 部署 llm 面已由 llm-deepseek 承载——多 provider 路由是部署面扩展（逐 provider 直写 adapter），非 dsh 核心缺口，按需再立）。
- **受阻/后续 9 行（按族，证据在案，不硬凑）**：sandbox 族 3 行（`dsh-sandbox-local` 原生执法基底：Linux `@deepseek-ai/node-addon-landlock-run` 原生 addon+macOS Seatbelt profile+Windows ACL 限制令牌运行器——Go 离线工具链无 cgo，landlock/限制令牌路径无法本机门禁验证，属 OS 原生工程轮；bash/pwsh-sandbox 是其上的薄执行器随基底同轮；Go 侧 sandbox-policy/fs-sandbox 词法+身份围栏与模型面文件工具执法已就位，缺的是 shell 进程级执法，如实保留缺口）；goal 族 5 行（goal 服务=会话日志 `goal/change` 事件流+fold 折叠的持久完成目标，配 tool-goal/command-goal/goal-round-driver/tool-ralph——纯 Go 可移植 epic，按 服务→tool-goal→command-goal→round-driver→ralph 依赖序独立轮推进）；session-telemetry-otel 1 行（OTel SDK 新 go.mod 依赖，离线工具链无法拉取验证，待联网轮盘点）。

### 剩余面依赖序地图（r37 盘点，实证自官方源）

- **web 家族**（web 缝 → web-fetch-http → web-search-deepseek → tool-web）：**已全清（双线合并）**——`web` 包（远程线：search/fetch 双注册表、执行期六分支选择、五码文案逐字）+ `webfetchhttp` 包（远程线：公网地址钉扎/同源重定向/双帽/charset 解码/取消三分类，x/text 依赖）+ `websearchdeepseek` 包（本地线并入：Anthropic 兼容 Messages + 原生 `web_search_20250305` 服务端工具、凭证链三段、redirect 拒跟、citation 映射，provider id `deepseek-official`——自此 base 钉选可达）+ `toolweb` 包（本地线并入：模型面 `web_search`/`web_fetch` 工具、多查询并发合并、fetch 三级截断、免依赖 HTML→markdown，prompt section order 2000/2100）。本地线曾并行的 `webs`/`websfetchhttp` 重复实现已在整理中弃置，采远程超集。
- **title 家族**（session-title 37.7KB → session-title-llm 12.8 + all-prompts-llm + first-prompt-llm）：**已清零（双线 2026-08-30 同日落地，合并时采远程完整版）**——远程线交付 `sessiontitle` 全版（provider 单槽+fallback+自动调度+rename 钉扎+supersede）与 `sessiontitlellm`（first-prompt provider）；本地线交付的基座在合并中被远程超集替换，其贡献面（校验/折叠/词数帽/空白拒绝）由远程版承继，webhook SessionCreator 的 `rename` 接线保留并经适配验证。
- **sandbox 家族**（r48 深盘定谳）：`dsh-sandbox-local` 执法基底三路皆 OS 原生件——Linux landlock 原生 addon（`@deepseek-ai/node-addon-landlock-run` launcher+functional probe）、macOS Seatbelt profile 参数、Windows ACL 限制令牌运行器（write SID 授予、只读探测、部分执法报告）；Go 离线工具链（无 cgo/gcc）无法承载 landlock/限制令牌路径的本机验证。bash/pwsh-sandbox 为其上的薄执行器（extends LocalBashExecutor + require ctx.sandbox provider）。Go 组装已带 `dsh-sandbox-policy`（mode+workspace root，fail-safe 只读）与 `dsh-fs-sandbox`（fs 后端换装+词法/身份围栏），shell 走 bash-local/pwsh-local 并机会式读 policy——缺的是 shell 进程级执法 provider（原生件），不是 policy 缝。
- **goal/ralph/workflow 家族**（goal 会话日志持久完成目标：服务 epic=domain/fold/index（`goal/change` 事件流+fold 折叠+生命周期）→ tool-goal + command-goal + goal-round-driver + tool-ralph；纯 Go 可移植，按依赖序每轮一子件；workflow-worker-thread/tool-workflow 已处置）。
- **webhook SessionCreator 组合**（①项）：**已落地（2026-08-30）**——四缝全补：agentPresets.mount（preset.Mounts 组合面 + standingKeyFor）、session-title（`sessiontitle` 包）、agent-default-model（r34）、workspace/permissionPresets/CreateAgent 在先；`webhook/session.go` 逐字移植官方 session.ts（resolveRequest 文案逐字、事务序列、installInitialModelSelection 监听、attach 后失败 detach+dispose 回滚只 warn）；catalog `dsh-webhook` 行 Inject 六服务与官方清单逐字，组装测全绿。
- **session-query-sqlite**（63.8KB）：**已落地（本地线并入，r45）**——`sessionquerysqlite` 包：FTS5 派生读模型 store 半体（modernc.org/sqlite 纯 Go 驱动已在 go.mod），懒打开（官方 base 挂载 path ":memory:"、openAt "never"——首次消费方使用才开库）、Dispose 链关闭；catalog `dsh-session-query-sqlite` 行（Provide sessionQuerySqlite）；同轮 toolsjobs 完成投递测试竞态修复（锁内快照访问器替代裸字段读）。
- **typert-loader / plugin-package-inventory-deepseek / llm-pi-ai**：待逐个盘点（后者 189KB 疑 N-A）。（webhook、api/gateway Remote 网关面——原"webserver UI dispatch 适配器"的精确残余（commands Execute 面已在，网关经 typert lookups 解析 agentId 参数；host/webserver 路由/升级/index 注入已落地，gzip 缓）、ACP、PTC、结构化捕获轮、web 家族/session-title/checkpoint 等重件），profile patchReload watch/live 后半轮，插件管理 CLI（`dsh plugin --profile … add`）随 CLI 轮。**账实对齐 2026-08-29**：官方树无 git context 插件（无包、无 npm 名，README 旧尾巴已清除）；typert 组 = 构建期生成器 + 运行时 registry + loader 集成 + Remote 协议——Go 侧 registry 的有意义消费方仅 api/gateway+remotes，registry 本体与网关同轮按需落地，不预造无调用者服务。**webhook SessionCreator 受阻盘点（2026-08-29，如实记录不硬凑）**：官方 createWebhookSession 事务（packages/webhook/src/session.ts）依赖四项 Go 侧未移植缝——① agentPresets.mount（preset cordis.yml 组合挂载进 agent 作用域 ctx，Go Roster 仅 Resolve/ResolveMountable 无 mount）② session-title（sessionTitle.rename，Go 无 title 包）③ agent-default-model（ctx.agentDefaultModel.currentSelection() 兜底选路）④ standingKeyFor（preset 的持久压缩键）；待四缝按依赖序落地后组合 webhook 条目（webhook 规则运行时本体+SessionCreator 缝已就位）。**受阻盘点 2026-08-30（r28 续，如实记录不硬凑）**：attachment-local——Go `attachment` 包只有 admission 词汇+Store 接缝，本地文件 Store 实现未移植（官方 ~50KB）；session-query-sqlite——Go `sessionquery` 只有 SearchBackend 接缝+引擎，sqlite 后端未移植；sandbox-local——Go `sandbox` 可写根推导已在，roots 服务 catalog 组合随 fs-sandbox 族轮；timer/hmr——cordis 核心插件（hmr 为 node 特定），Go cordis 无对应物；goal/ralph 族、web 抓取族（web-search-deepseek/web-fetch-http/tool-web）、tool-subagent/tool-subagent-report、tool-workflow、session-title(+llm)、agent-default-model、llm-retry、llm-pi-ai、plugin-package-inventory-deepseek、session-telemetry-otel、typert-loader——无 Go 包，随各自依赖轮；workflow-worker-thread——Go `workflow` 引擎替代 worker-thread JS realm（R10 决策），catalog 行随 tool-workflow 轮以 Go 引擎补挂。
2. **未移植面**：ACP provider、api/gateway Remote 流载体面（stream-protocol/mux/forwarded events + client/* 随 stream carrier 轮——Host 派发核心已落地）、PTC `run_code` 传输与执行基底（**受阻盘点 2026-08-29**：`coderuntime` 缝已落地；传输面 ptc.ts 依赖 CodeRuntime 后端——官方 TS 后端是 node worker_threads + 原生类型剥离，本机无 node、Go 侧亦无进程内 JS 基底决策，待基底轮定夺后传输/SDK section/后端按依赖序跟进；Python 后端协议同轮评估）。（subagent list-children 已随 `subagentcontrol` 的 list_agents 落地；token-meter O(1) 投影单元与 turn-usage 折叠已落地——llm-retry 插件未移植，`llm/retry(-started)` 词汇仅作只读折叠预留；compaction-pruner 已随 `toolresultpruner` 落地——Engine 可选缝已接线；context tmux 已随 `tmuxcontext` 落地；git context 官方无此物——账实对齐清除。）
3. **文件工具家族依赖根已移植**：`fs`（官方 dsh-fs seam 全量词汇：不透明 TargetKey/Version 身份、Observation/Info/PathInfo/DirEntry、WriteIntent 守卫（createIfAbsent→FS_NOT_OBSERVED / replaceIfVersion→FS_STALE_VERSION）、Write/EditOutcome 的 before/after LF 规范化 diff 基、13 个 FS_* 稳定错误码 + FsError 类型、fs/write-intent·edit-intent·observed 三个 cordis 事件名、FileSystem 接口含 sandboxMode 能力位与 per-call SandboxExecutionPolicy 缝）+ `fslocal`（本地后端：realpath 身份+缺失后缀祖先行走键稳定、NUL+UTF-8 二进制拒绝、64KiB 流式解码含拆分 rune 回退、ReadBytes 上限在打开描述符上执行 FS_TOO_LARGE、稳定名序 ListDir、写/改 per-key 锁串行化读→守卫→写窗口、editText 版本守卫先于字面匹配、old_string 唯一性纪律（0→EDIT_NOT_FOUND / >1 无 replace_all→AMBIGUOUS_EDIT）、原子发布同目录 temp+rename 保模式；诚实降级：可移植 os.FileInfo 无 dev/ino/creation，version token 退化为 size+mtime——token 不透明故对消费者不可见）。官方 86 名单无裸 dsh-fs/dsh-fs-local 条目——catalog 接线随 dsh-fs-sandbox/dsh-fs-observation-policy 轮；tool-str-replace-editor（528 行）**已在其上落地**（`strreplaceeditor` 包：view（cat -n 行号渲染+view_range [start,-1] 纪律+目录两级树排除 dotfiles/node_modules/__pycache__）、create（已存在即 plain 拒绝）、str_replace（歧义报 FS_AMBIGUOUS_EDIT 且列出全部命中行号、未命中报 FS_EDIT_NOT_FOUND、json-null new_str 拒绝）、insert（insert_line∈[0,lines]）；写路径经 fs/write-intent·fs/edit-intent 单槽 waterfall 决策 + fs/observed 观察记录（Go 适配：三事件均走 ctx.Waterfall）；绝对路径纪律带修正提示；MutationPolicy 语义保留——backend 不设 sandboxMode 时 policy 可缺位，设了而服务缺位则组合 bug fail-loud）+ catalog 条目 tool-str-replace-editor（Inject tools/fs/agents；fs 服务由 fs-sandbox 轮提供，缺位即 inject 期 fail-loud——组合纪律而非缺口）；tool-fs（1416）/tool-fs-search（1486）在此地基上分期。
4. **沙箱策略与围栏（官方 dsh-sandbox/roots + dsh-sandbox-policy + dsh-fs-sandbox 三家）**：`sandbox` 包（可写根推导的唯一家——workspace-write = 工作区根 + /tmp + 用户平台临时目录，canonical 化+去重；词法包含快路径 Windows 大小写不敏感、目录边界前缀不误配）+ `sandboxpolicy` 包（部署默认 mode+回退工作区根的唯一所有者；解析优先级 approved > session 最后 sandbox/mode 覆盖 > 部署默认，session cwd 即 workspace-write 边界、缺位落配置根；空 mode fail-safe 为 read-only；Go 适配：Resolve 取显式输入——session cwd/覆盖/批准 mode 三参，executor 无会话态；sandbox/mode 事件词汇与折叠除本就在 permissionpresets，策略服务消费之）+ `fssandbox` 包（本地后端全量机制原样承袭、仅对两个写操作加 per-call policy 围栏：read-only 拒绝、workspace-write **当场再 canonical 化**并要求可写根包含（捕捉工具解析后换掉的符号链接祖先；TOCTOU 残留如实按威胁模型接受）、danger-full-access 不设防；包含检查=词法快路径+文件系统身份回退（os.SameFile 祖先行走，认 Windows 8.3 别名与大小写）；拒绝抛 FS_SANDBOX_DENIED 带模式文案）。catalog +2（累计 34）：dsh-sandbox-policy（Provide sandboxPolicy）、dsh-fs-sandbox（Inject sandboxPolicy、Provide fs——装它而非裸 local 即全量换装，模型侧工具不动）；tool-str-replace-editor 条目按需挂 mutationPolicyResolver（agent.Session 头 cwd+knob 折叠→service.Resolve），policy 服务缺位且 backend 不围栏时仍可工作。集成测验证：workspace-write 下根内 create 落盘、盘根 create 拒绝且编辑器包沙箱拒绝标记。
5. **tool-fs 工具族 + 沙箱升级词汇（官方 dsh-tool-fs 核心 + dsh-sandbox/escalation 全量）**：`sandbox/escalation.go`（两执行族共用的升级词汇唯一家——严格更宽阶梯 WIDER_MODES（read-only→workspace-write/danger、workspace-write→danger；封顶不再升）、闭集 ESCALATION_TARGETS、参数配对校验（permissions/justification 同进同出+非空句）、**逐字标记**：`[sandbox: file access denied under <mode> mode]` + 同轮升级提示标记、ApproveEscalation 有序 fail-closed 序列：先严格更宽检查（执行期检查而非 schema 约束）→ 无审批服务报错 → 无 agent fail-closed → 审批 ask（理由自含审计：`escalate sandbox to <mode>: <理由>`）→ 四值结果映射各自逐字报错，rogue 值 fail-closed；非更宽请求绝不打扰人类）；`toolfs` 包（read/write/edit 三工具：read 走 stat 路由→大文件/未知大小**流式**、buildWindow 行+字节双帽扫描仍数出精确总行数、单行截断标记、offset 越 EOF 报 FS_NOT_FOUND 官方文案、渲染 `<path>..</path>` 信封+续读 footer、langFromPath 扩展名提示；write 单槽意图 waterfall 默认无条件原子写、Created/Updated 文案、before 可空 oneOf schema；edit 字面唯一匹配（fslocal 纪律：0→EDIT_NOT_FOUND/多匹配→AMBIGUOUS_EDIT 带 more-specific 指引）、stale/not-observed 附补救文案（re-read/read the file then retry——FS 码保留）、executing agent 的 session cwd 为解析基（`..` 穿越即 canonical 化）、观察记录 fs/observed；执行体 canonical 值纯 JSON 形状（[]any/map[string]any，指针拒绝——lossless JSON 纪律）；**escalation 字段仅围栏 backend 挂载时进 schema**（validator 先拒）、FS_SANDBOX_DENIED→共享 marker+同轮提示、审批通道经 userapproval.Service.Request 适配（四值词汇与官方同构）；read_image 按**源规则自身**不注册：需 attachments 存储，Go 组合未挂载——如实记录）。catalog +1（累计 35）：dsh-tool-fs（Inject tools/fs/agents/systemPrompt/sandboxPolicy/userApproval；挂三段 systemPrompt 指引 section，order 1100/1200/1300）。集成断言：围栏组合下 write schema 带 sandbox_permissions/justification 枚举。
6. **subprocess 执行缝（官方 dsh-subprocess 契约 + dsh-subprocess-local 本地实现）**：`subprocess` 包——完全指定的 spawn 请求（argv 不经 shell、cwd/stdio/grace 全显式无默认，缺一 fail loud）、Node 形 stdio 逐流处置（stdin ignore/pipe/{data 批量写后关}；stdout/stderr pipe=裸流归调用者/inherit/collect）、**有界收集输出**：内存尾帽（溢出丢头保尾——错误与结果聚在输出末尾的 pi/OpenCode 理据）+ 可选全流 spill 文件（O_EXCL+随机名防预测防符号链接种植、超全流帽即弃 spill 断告、整流字节坐标 offset 游标零读指针——独立读者互不吞噬、lossy 读报截断并指路 spill）、spawn 上下文取消即 terminate、**树域终止**：POSIX setpgid 分离进程组 kill(-pgid) 组信号带直接子代回退（TERM→graceMs→KILL 阶梯；存活探针 kill(-pid,0) 仅 ESRCH 判死、EPERM 判活）、Windows taskkill /T /F 立即强杀（阶梯塌缩、结果有意不查——与 ESRCH 同等的容忍）、树退出观察者首个确认缺席即永久不再发信号（防 pid 复用误杀）、**drain 边界**：进程退出后幸存后代持有的继承管道由同一 grace 封顶（仅 harness 收集管道强关，pipe 模式流归调用者）、环境基座 scrubbedParentEnv（DSH_* 与 KEY/PASSWORD/SECRET/TOKEN 凭证形名全部剔除、大小写不敏感匹配 Windows 语义；显式 env 字符串=有意 opt-in 可还原凭证/DSH 事实、nil 墓碑=删普通环境项）；Go 适配：平台文件分治（tree_posix/tree_windows）、`SysProcAttr.Setpgid` 分离、exit 事实走 close 事件词汇（ExitCode/-1+Signal）、Runtime 接口 + Local 实现。官方 terminal 原语（pty 分配、前台组检视、win32 进程检查器）为**文档化缓期**非静默缺口——piped 实现已覆盖 batch/流式消费者。catalog +1（累计 36）：dsh-subprocess（Provide subprocess）。测试：批量 stdin 往返、管道 stdin 面、树终止阶梯（helper 进程 spin 循环 + 幂等再终止）、上下文取消联动、spec 逐项 fail loud、裸流归属、环境 scrub/墓碑、尾帽字节精确截断、spill 生命周期（创建→超帽弃→seal 后路径仍广播）。
7. **fs-search 发现工具族（官方 dsh-tool-fs-search：glob + grep + search-core）**：`fssearch` 包——两工具皆为经 subprocess 缝的前台固定 argv spawn（无 shell 层无引号问题：模式/include 走 `--flag=value`、目标躲在 `--` 后，前导 `-` 值永不成旗标；`--no-config` 前置防宿主 RIPGREP_CONFIG_PATH 注入 `--pre` 预处理器）；glob=`rg --files --glob=<模式> --sort=modified --no-ignore --hidden` + **每 VCS 名双否定 glob**（裸形遍历剪枝 + `/**` 形防搜索根恰在 .git 内时失效）、修改时间序契约（装得下就整列原样展示）、超帽页两种形态：修改时间头 or **跨顶层条目轮转采样**（每条目先得一席、桶内按轮追加、桶序随首现——展示页与卡片同法同算不分歧）、footer 三态（平尾/sampled-across 带收窄提示/完整结果可存 spill 时的恢复定位）；grep=行导向 `rg --json` NDJSON（path/line_number/lines 三字段无冒号切分歧义）、begin/end/context/summary 记录为传输框架跳过、**malformed 即 SEARCH_FAILED 绝不部分结果**、非 UTF-8 行（base64 bytes）出占位预览不炸全局、按文件首现序分组 `Line N: <text>`、行预览 UTF-8 边界保安全截断+`(line truncated)` 标记、`Found N of M matches` 预算事实头、include 纪律（空白拒绝/否定式拒绝/顶层逗号拒绝而 `{a,b}` 花括号交替合法）；错误词汇 SEARCH_INVALID_PATTERN（regex parse error/error parsing glob 分类）/SEARCH_FAILED（launch 失败/信号击杀/畸形输出）/SEARCH_RAW_OUTPUT_OVERFLOW（lossy 或超 rawOutputMaxBytes——**宁清晰失败不解析静默残流**）/SEARCH_ABORTED，exit 1=成功零结果、exit 0=有结果；session cwd 为 workdir、显示路径 workdir 相对化（界外原样透传——v1 部署要求 workdir 与 fs read 根同工作区，文档化非运行时校验）。**Go 部署适配（诚实记录）**：官方打包 @vscode/ripgrep，Go 缝从 PATH 惰性解析 rg（一次/进程、rgPath 配置可钉死）——缺二进制在首次调用 SEARCH_FAILED 而非 Loader 组装失败（与官方 call-boundary 解析同语义）；集成测在无 rg 部署上**如实 skip**（本机无 rg：spawn 全链路由部署验证）；spill 恢复面（trySaveFormattedResult）Go 工具臂仍缺——spillStore 服务已随 r27 storage/spill 组装轮接入，但 Go Render 面无 exec/ctx 通道（官方在 view/projection 钩子带 exec 保存），缺席走 could-not-save 文案与官方可选语义同形，随展示轮与 search 卡片（presentationMeta）一并接线。catalog +1（累计 37）：dsh-tool-fs-search（Inject tools/subprocess/systemPrompt；挂 tool:glob 1400/tool:grep 1500 指引段，超帽文案随采样开关）。
8. **shell 契约 + 受管环境注册表（官方 dsh-shell + dsh-shell-env：bash/pwsh 执行器缝的地基）**：`shell` 包——执行契约词汇（ShellExecRequest→ShellExecutor.Resolve→ShellExecSpec 的两段式：workdir/timeout 由实现默认+封顶后填充；Run **只对基础设施失败报错**——非零退出/超时击杀/中止击杀都是描述性结果；Aborted 与 TimedOut 互斥、单一融合 deadline 报首因不双报；Start 立即返回且无超时、done 永不报错、spawn 失败结为 killed+stderr 错误；ReadOutput 增量消费永不重发、lossy 指路 spill；后台进程归属组合拆卸边界=subprocess 服务 disposal、执行器级 reload 不杀）；受管环境合并序（先洗凭证、再请求方 env、最后 DSH_* 快照封顶——环境事实永不被顶替、宿主陈值永不透传）；ShellSandboxInfo 与退出状态独立上报（命令失败≠策略拒绝≠runner 失败）；ParseExitStatus 复演端出口药丸恢复（`[exit code: N]`/`[killed by signal: X]` 标记的正文剥离，消吃避免双重渲染，timeout/sandbox 标记留正文，要求前导换行+串尾防止 marker 样正文误匹配）。shell-env 注册表：内建事实 DSH_HOME/DSH_SHELL/DSH_SESSION_ID 注册表自持、插件贡献者声明键集（名字/键唯一、内建键保留、键形 `DSH_[A-Z][A-Z0-9_]*`、描述非空——全量 fail-loud 先于首命令）；collect 按名序解析、返回未声明键 panic 炸场；list 枚举不执行解析器。catalog +1（累计 38）：dsh-shell-env（Provide shellEnv；dshHome 配置）。**诚实记录**：DSH_SESSION_JSONL 持久化贡献者随 Go session/persistence 组合落地；shell 服务本体（bash-local/pwsh-local 执行器、tool-bash/tool-pwsh 工具面）按官方 win32 互斥装配——执行器两条已随条目 9 落地，工具两条已随条目 10 落地。
9. **本地 shell 执行器（官方 dsh-bash-local + dsh-pwsh-local：`ctx.shell` 的两个提供者）**：`shelllocal` 包——公共命令 `bash -c` / PowerShell（UTF-8 双向 preamble + `-NoLogo -NoProfile -NonInteractive -Command`；可执行文件解析=pwshPath 钉死→PS7 已知位置→PATH 条目（setx 式整条引号剥离、lstat 语义看得见 Store 别名不踩 ACL）→5.1 兜底→POSIX 交 PATH）；请求/规范两段（缺省 cwd=config→进程 cwd、timeout 缺省=封顶；stdoutMaxBytes 信任内进程消费者直传）；**超时/中止单首因分类**（CAS 记先到因，双臂竞态只报一个）；后台句柄=立即返回、无超时、done 永不报错、信号终止（含自杀）结 killed、spawn 失败结 killed 且失败注记**恰一次**经读路径送达（与真 stderr 互斥）；读路径增量消费永不重发、stderr 以 `[stderr]\n` 分节、节间单换行补齐；模型友好终端环境（NO_COLOR/TERM=dumb/PAGER/GIT_PAGER）层序=覆盖先行→调用方 env→DSH_* 快照封顶（受管事实不可顶替）；bounded collect+spill 上限每 spawn 显式供给（后台进程跨执行器 reload 仍受管——拆卸边界在 subprocess 服务 disposal）。catalog +2（累计 40/86，余 46）：dsh-bash-local + dsh-pwsh-local（皆 Provide shell；官方语义一宿主一提供者、双装 fail-loud，win32 层换行——本机 headless 实跑走 pwsh 行）。**测试部署适配（如实）**：Windows 的 PATH "bash" 是 WSL stub（官方 tool-bash 亦 win32 禁用）——bash 生命周期真跑测仅 POSIX 生效，win32 以 pwsh 执行器跑同套真 spawn 生命周期（运行/超时击杀/中止分类/后台增量读/取消击杀，本机全过）。
10. **模型面 shell 工具（官方 dsh-tool-bash + dsh-tool-pwsh：`bash`/`pwsh` 工具与 `tool:bash`/`tool:pwsh` 指引段）**：`shelltool` 包——一个注册函数按所组合执行器的 flavor 派生工具身份（`Name()=="pwsh-local"` → `pwsh`/OrderToolPwsh/`[exit code: N]`+裸 exit 1 即中断的指引段，否则 `bash`/OrderToolBash）；参数面 command/description（必填 trim 非空）+ timeoutMs（正数校验逐字 `invalid timeoutMs: expected a positive number, got …`）+ workdir（模型相对路径接会话 cwd——调用方 agent 经 scope→Agent 解析器取 `Session.Header().CWD`，缺省即会话根）+ `run_in_background`（默认开，`enableRunInBackground: false` 时**不进 schema 且调用拒绝**逐字禁用文案）；执行体=Env.Collect（受管 DSH_* 快照入请求）→ Resolve→Run（Signal=调用方取消）；仅基础设施失败报错，非零退出/超时/信号都是描述性 canonical 值（`exitCode` 信号时 null、`signal` 空时 null、spillPath 非空才在、timedOut/aborted/timeoutMs/stdout/stderr{text,truncated,spillPath}，输出 schema OneOf background{kind,jobId}/foreground）；渲染=stdout→`[stderr]` 分节→尾部标记阶梯（timedOut→signal→exit code，exit 锚最后）+ 截断指路 spill（缺路径报 `(unavailable)`）；后台经 jobs 缝：StartSpec{kind bash/pwsh, label=命令, owner=调用方 agent 适配（无 agent 即 unowned）}、processOutcome 映射 killed→killed（signal detail）/其余→completed+exit detail（非零退出照 foreground 语义是报告不是失败）、无 jobs 服务报 `background jobs unavailable`、调用方取消预检→`tool call aborted`。catalog +2（累计 42/86，余 44）：dsh-tool-bash + dsh-tool-pwsh（Inject tools/shell/systemPrompt/shellEnv/jobs/agents；共享 buildShellTool 工厂；jobs 为可选注入——缺位即 pending 等待而非静默）。**诚实记录**：官方 escalation 子句（sandbox_permissions/justification 与 confining 执行器沙箱通知）不移植——Go 组合无受限 shell 执行器，字段不进 schema 标记不可达；`Cancel` 钩子为空操作（Go 后台进程无独立 kill 谓词，停止走 job Kill 通道的 spec.Signal）；boot 组装测按宿主条件装配恰好一行（win32=pwsh-local+tool-pwsh，POSIX=bash-local+tool-bash），双行装配被 prompt section 重名 fail-loud 挡住——组合纪律的测试化。
11. **插件 ABI**：宿主端 TS 插件改为受管子进程（stdio + JSON-RPC），TS 插件零改动迁移。

---

# alpha.2 移植路线（dsh-v0.1.2-alpha.1 → dsh-v0.1.2-alpha.2）

> 依据：上游 `git diff dsh-v0.1.2-alpha.1..dsh-v0.1.2-alpha.2`（234 提交，1604 文件，+27862/-14050），
> 逐提交语义溯源（2026-08-31 维护轮产出）。上游 checkout：`E:\code\nodejs\deepseek-harness` @ 0a53fb55be。

## 结论速览

- **wire 破坏性变更 3 项**（W1-W3），**行为变更 6 项**（B1-B6），**纯结构 0 动作**，**13 个族零 delta**。
- projection **核心零语义 delta**（发布分支实现胜出，master 迭代被覆盖；唯一差异是一段注释）。
  迁移实质全在**消费方**：事件日志扫描 → 注册投影单元读取。
- Team 家族 = alpha.1 已有 `experimental/agent-team` 的原位演进 + 重命名（team→agent-team，
  tool-team→tool-agent-team），非新家族。

## W：wire 破坏性变更（Go 必改）

### W1. session 信封 `ignorable?: true`
- 上游：`SessionEvent` 信封新增 `ignorable?: true`（types.ts）；known-event 守卫语义变更——
  未识别事件类型若携带 `ignorable: true`，读端**可跳过**；缺席仍 fail-closed（拒绝而非腐化）。
- 上游**明确否决事件名注册机制**（known-event-types.ts 注释：registration "does not classify
  omission safety and would make reads composition-dependent"）；佐证链含一次 revert 往返
  （2c6ff296af 恢复 ignorable 外部事件）。
- Go 改造面：`session/types.go` 信封校验（seed 路径接受 ignorable 且仅接受 true）、
  persistence 读路径守卫按标记跳过、sessionquery 折叠容忍。
- **决策待入账**：dsh-go 现有 `RegisterEventType/EnsureEventTypes` 与上游否决立场冲突——
  需决策：保留（宿主静态装配下的等价机制）或废弃对齐上游。倾向：保留 EnsureEventTypes
  供装配层用，读路径守卫改为 ignorable 标记权威（两种机制并存时标记优先）。

### W2. gateway 错误码命名空间化
- 上游：17 个错误码 `x` → `gateway/x`（remote-error-codes.ts 新文件），迁入共享
  `RemoteErrorDetailsMap`，每码携带类型化 details `{ endpoint, field? }`；`RemoteError`
  类带 `isDSHRemoteError` 结构标记（跨 realm 识别，Go 侧无对应需求）。
- Go 改造面：`gateway` 包错误码词表逐字改名 + details 载荷；`typert` 协议错误细节表对齐。

### W3. 新增 `session/not-found` 远程错误细节
- 上游：SessionId 解析层的标准失败细节 `{ sessionId }`（types.ts 模块扩充）。
- Go 改造面：gateway/session 控制面错误词表补一项。

## B：行为变更（Go 对应站点）

### B1. atomic-write Windows 重试语义精化
- 上游：`EACCES/EBUSY/EPERM` 瞬态错误重试，20ms 起 ×2 指数退避帽 200ms，8 次上限；
  仅 win32；最终失败删 temp 重抛。
- Go 现状：`settings/file Save()` 已有重试（任意错误 10×50ms 固定）——**语义偏差**；
  `storagejson` 原子发布**无重试**。
- 动作：抽共享原子替换助手（建议 `fsutil` 或新 `atomicwrite` 包）对齐上游语义，
  settings/file 与 storagejson 两处接用。

### B2. turnBoundary 投影单元（核心新增）
- 上游：agent-loop 注册 `turnBoundary` 单元（stateVersion 2），折叠 turn/start、turn/end、
  step/start、step/end → `{openTurnStartSeq, lastStepStartSeq, lastStepBoundary, lastTurn}`。
- 消费方：hooks 双桥 turn 序号（替代事件扫描，`sessionProjections` 变必需注入）、
  agent.ts lastTurn。
- Go 改造面：`agentloop` 注册单元；`hooksclaudecode/hookscodex` 改读投影；
  `agent` 包新增 projection 定义文件。

### B3. goal 投影折叠增强
- 上游：goal 投影除 `goal/change` 外，还消费 `user/message` 中 goal 归因源
  （`source.kind === 'goal'` 且 goalId/revision 匹配）推进 `roundsStarted`。
- Go 改造面：`goal` 包（r49 刚落地）折叠函数补此分支 + 测试。

### B4. permission-presets 种子边界投影化
- 上游：1c2acd9157 "project the seed boundary" + 8645053ca0 require registry——
  种子期判定从折叠扫描改为投影单元；registry 成为必需接缝。
- Go 改造面：`interaction/permissionpresets` 投影单元语义对齐。

### B5. plan-mode / session-title 投影化
- 上游：plan-mode require registry（8645053ca0 + a6c7c70d4f）；session-title 输入改
  O(1) 投影（b0c2e2bf01）并保持 v1 cache schema。
- Go 改造面：`planmode`、`sessiontitle` 对应折改投影读。

### B6. agent-presets live-mount-first
- 上游：已挂载预设优先于其（可能已损坏的）文件——挂载即事实；mount 记录按 runtime
  fiber 作用域（Go 单运行时，fiber 维度 N/A 但排序规则可移植）。
- Go 改造面：`preset` 包 roster 健康判定顺序调整 + 测试。

## P：性能对齐（低风险，随轮携带）

- deque：上游 `util/deque` 替换 shift 队列（gateway 帧队列等）。Go 侧 slice 头部弹出
  热点待 profiling 确认后再定；`util/deque`（95 行）可作为移植参照。
- `util/time`（33 行）、`util/values`（239 行：JsonValue/assertNever/snapshotJsonValue/
  isJsonValue/deepEqualJson/deepFreeze）——Go 侧均有原生对应物，零动作。
- `perf(session-projection) skip history reads`：发布分支实现已在 alpha.1
  （`observedSeq >= throughSeq` 早退），零动作。

## Z：零 delta 族（13 个，无需跟随）

shell / fs / subprocess / sandbox / settings / storage / skill / compaction /
workflow / typert / sdk / boot / jobs / interaction / guard 的 src 在 alpha.2 无变化。

## 存量剩余面合并（与 r48 清单归并）

- 不变的存量：sandbox 执法（OS 原生轮）、PTC run_code（无执行基底决策不变——上游 alpha.2
  对 ptc.ts 仅 order 访问器重构，无新执行面）、ACP、OTel、Files API。
- 上游已有而 Go 未移植的存量候选（alpha.1 既有，非 alpha.2 新增）：terminal 家族、
  tool-bash-persistent/tool-pwsh-persistent、subagent provider packs（claude-code/codex/
  dsh-sdk/acp/in-process 等）、tool-session-query、goal-round-driver 已在 r49 落地。
- hooks 双桥注入面变化（B2）使 `sessionProjections` 成为必需依赖——移植 B2 时同步。

## 执行顺序（依赖序）

1. **W1 ignorable**（读路径守卫 + 信封校验 + 注册机制决策入账）——影响面最广先做
2. **B2 turnBoundary**（agentloop 单元 + hooks 双桥改造）
3. **W2+W3 gateway 错误码**（词表 + details + session/not-found）
4. **B1 atomic-write 语义对齐**（共享助手 + 两站点）
5. **B3 goal 折叠** / **B4 permission 种子边界** / **B5 plan+title 投影化** / **B6 preset live-mount-first**（互相独立，可并行轮）
6. 每轮按项目纪律：语义决策入账、全绿门禁、诚实降级显式记录

## 环境基建（本轮已落）

- C 工具链：WinLibs gcc 16.1.0（winget BrechtSanders.WinLibs.POSIX.UCRT）——老 gcc 8.1.0
  与 go1.26 race runtime 不兼容（0xc0000139）。README 工具链记录待 A1 更正。
- 全量 `go test -race`：103 包三块全绿零 race（修复 7 包后）。
- 用户级 TEMP/TMP 改长路径（8.3 短路径 `HZ0704~1` 曾致 6 包预存失败）。


## 移植轮执行状态（2026-08-31 维护轮）

| 轮 | 内容 | 状态 |
|---|---|---|
| 1 | W1 ignorable 信封（字段+wire 往返+false 拒绝+读路径标记权威+coordinator 测试） | ✅ 完成 |
| 2 | B2 turnBoundary 投影单元（agentloop 注册+NewAgentLoop 必需接缝+hooks 双桥改读投影+catalog Inject） | ✅ 完成 |
| 3 | W2+W3 gateway 错误码 gateway/ 前缀化 + WireFailure 修 internal 塌缩缺口 + typed details 上 wire + typert.CodeSessionNotFound | ✅ 完成 |
| 4 | B1 atomicwrite 包（上游节奏 20ms→200ms 指数 8 次，win32 瞬态集）+ settings/file 与 storagejson 两站点接用 | ✅ 完成 |
| 5 | B3 goal 折叠增强 | ✅ 核实已被 r49 满足（goal 包落地晚于 alpha.2，fold 已含 goal-source rounds 语义） |
| 6 | B4 permission 种子边界 / B5 plan+title 投影化 / B6 preset live-mount-first | ⏳ 待逐项核实远程线并入状态（r40-r49 合并可能已带部分语义） |

**开放项**：F12 shelllocal 全量并发下 spawn 时序 flake（单跑/分块全绿；需专门的时限余量容差轮）。
## 轮 6 执行状态（2026-08-31 续）

| 项 | 结果 |
|---|---|
| B4 permission 种子边界 | ✅ 单元已在（远程线 r39+），本轮补 permissions 单元 catalog 注册 + Inject 接线 |
| B5a plan require-registry | ✅ plan 单元生产注册（原为死代码）+ Controller 状态读改投影（NewController 必需接缝） |
| B5b title O(1) 投影 | ⏳ 行为等价、纯性能内部项——与 deque 同类延后待 profiling |
| B6 preset live-mount-first | ✅ 架构性 N/A 入账：Go 静态单运行时组合下，挂载后文件损坏不影响已组合运行时（无 HMR 多 fiber 面），上游场景不存在 |
| dsh-command-goal | ✅ 包已在（r49），本轮补 catalog 行（真实缺口系漏接线） |
| dsh-tool-ralph | ⏳ 唯一真未移植件（439 行工具插件，workflowEngine+subagents 均在位）——独立轮，下一批首项 |
| catalog 精确账 | 82/85（base 名单 85 非 86；处置 7 + 受阻 4；真实缺口=tool-ralph 1 件）。genstatus 计数修正为全键形态 |
## 轮 7 执行状态（2026-08-31）

| 项 | 结果 |
|---|---|
| session-telemetry seam + coordinator | ✅ 移植完成（live+on-demand 双模式/分块投影/游标/脱敏瀑布/agent-error 中继/shutdown 标记） |
| session-telemetry-otel 后端 | ✅ 移植完成（OTel v0.22 log SDK 组合/severity 映射/shutdown 时限/DISABLED 零 SDK） |
| catalog 接线 | ✅ dsh-session-telemetry-otel 条目（三模式 + ServiceTelemetry） |
| OTel Go SDK 依赖族 | ✅ 引入（决策入账，见 DECISIONS.md） |
| sandbox 家族 | ⏳ 下一批（sandbox-local + windows-acl + bash/pwsh-sandbox，OS 原生执法层） |