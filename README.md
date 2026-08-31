# dsh-go — DSH 宿主的 Go 重写工程

对照官方 `deepseek-ai/deepseek-harness` 标签 **dsh-v0.1.2-alpha.1** 的源码语义，把宿主运行时从 TypeScript/Node 重写为 Go；Web 客户端与插件 SDK 契约保持 TypeScript。语言分界画在 wire 契约上，不在实现上。


## 文档导航

- [STATUS.md](STATUS.md) — 已完成包账本 + 程序化生成的门禁计数
- [DECISIONS.md](DECISIONS.md) — 语义决策记录（每条可追溯官方）+ wire 契约冻结清单
- [ROADMAP.md](ROADMAP.md) — 剩余面清单、依赖序地图、alpha.2 移植路线
- [FINDINGS.md](FINDINGS.md) — 审查发现台账（R 编号闭环）
- [VERIFY.md](VERIFY.md) — 验证门禁与双 Agent 协调

## 构建

工具链：本机 go（go1.26.5）+ WinLibs gcc 16.1.0（`winget install BrechtSanders.WinLibs.POSIX.UCRT`）。GOPROXY 已配置 goproxy.cn 优先。

```powershell
go build ./...
go vet ./...
go test ./... -count=1
```

-race 已可用（CGO 经 WinLibs gcc；旧 gcc 8.1.0 与 go1.26 race runtime 不兼容，勿回退）。
TEMP/TMP 必须为长路径（8.3 短路径会破坏 canonical 化断言，用户级已修正）。
