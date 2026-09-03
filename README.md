# dsh-go — DSH 宿主的 Go 重写工程

对照官方 `deepseek-ai/deepseek-harness` 标签 **dsh-v0.1.2-alpha.2** 的源码语义（基线 alpha.1，r50 起 alpha.2 增量已并入），把宿主运行时从 TypeScript/Node 重写为 Go；Web 客户端与插件 SDK 契约保持 TypeScript。语言分界画在 wire 契约上，不在实现上。


## 文档导航

- [STATUS.md](STATUS.md) — 已完成包账本 + 程序化生成的门禁计数
- [DECISIONS.md](DECISIONS.md) — 语义决策记录（每条可追溯官方）+ wire 契约冻结清单
- [ROADMAP.md](ROADMAP.md) — 剩余面清单、依赖序地图、alpha.2 移植路线
- [FINDINGS.md](FINDINGS.md) — 审查发现台账（R 编号闭环）
- [VERIFY.md](VERIFY.md) — 验证门禁与双 Agent 协调
- [webassets/README.md](webassets/README.md) — 前端构建产物布局与再生
- [frontend/UPSTREAM.md](frontend/UPSTREAM.md) — 前端源码 fork 的上游钉版（守卫 v5 比对源）

## 构建

工具链：本机 go（go1.26.5）+ WinLibs gcc 16.1.0（`winget install BrechtSanders.WinLibs.POSIX.UCRT`）。GOPROXY 已配置 goproxy.cn 优先。

```powershell
go build ./...
go vet ./...
go test ./... -count=1
```

-race 已可用（CGO 经 WinLibs gcc；旧 gcc 8.1.0 与 go1.26 race runtime 不兼容，勿回退）。
TEMP/TMP 必须为长路径（8.3 短路径会破坏 canonical 化断言，用户级已修正）。

## 前端构建与同步流程

前端保留官方 TypeScript 栈（Owner 裁决），Go 宿主只消费 wire。两条同步路径：

1. **源码 fork**（`frontend/`，随本仓走）：`scripts/sync-frontend.ps1 -Monorepo E:\code\nodejs\deepseek-harness`
   拷贝官方 apps/web + web-app 依赖闭包源码（84 包 @deepseek-ai，去 node_modules/lib/dist），
   写 `frontend/UPSTREAM.md` 钉版（上游 commit + tag + 时间戳 + 包数）。
2. **构建产物**（`webassets/`，Go 服务直接读）：`scripts/sync-webassets.ps1 -Monorepo …`
   从官方源码构建 vite dist + 各 dsh.client 包 lib/client.js，按锚点布局固化。

**守卫 v5**：`boot/frontend_guard_test.go` 读 UPSTREAM.md 钉版 commit，与官方 clone 当前
HEAD 比对——漂移即 fail（13 天 webassets 过期事故的制度防线）。clone 缺席时跳过并记录
（设 `DSH_UPSTREAM_CLONE` 可覆盖 clone 路径）。何时跑 sync：上游升级或改前端后，先 sync
前端源码与产物，再提交（守卫在门禁期拦截漂移）。
