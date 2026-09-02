# webassets — 前端构建产物（从官方源码构建）

本目录是 deepseek-harness 前端的构建产物，按 Go 宿主的锚点布局固化。
前端保留官方 TypeScript 技术栈（Owner 架构决策）：不移植为 Go，也不使用
npm 上的官方预构建包——直接从官方源码仓库构建，产物随本仓库走，与 Go
后端的 wire 协议版本恒定一致。

## 布局

    webassets/
      package.json                    ← 锚点标记（cmd/dsh 缺省解析）
      node_modules/@deepseek-ai/
        dsh-frontend/dist/            ← apps/web 的 vite 构建（浏览器 shell）
        <client 包>/lib/client.js     ← 各 dsh.client 包的浏览器半体（boot graph）
        <宿主包>/cordis.patch.yml     ← bundle 行（loader 消费）

## 版本对齐

- 产物构建自官方源码 tag `dsh-v0.1.2-alpha.4`（与 Go 后端实现的 wire 协议
  版本一致：remote.mux 流协议、一元 POST /api 信封、$events 转发事件流）
- 浏览器 boot graph（`__DSH_BOOT__`）由 Go webhost 从本目录的 client 包
  扫描组合（`gatewaystream`/`webhost` 的 boot graph composer）

## 再生

上游 tag 升级或前端源码变更后，从官方 monorepo 重新构建并同步：

    pwsh scripts/sync-webassets.ps1 -Monorepo E:\code\nodejs\deepseek-harness

脚本完成：
1. web dist 构建（webworker-runtime / client-store 前置依赖 → vite build）
2. 所有 `dsh.client.platform=='web'` 包的 tsdown 构建（失败收集不中断）
3. 按锚点布局拷贝（package.json + cordis.patch.yml + lib/client.js，
   去除 source maps）

## 服务

cmd/dsh 缺省锚点解析：从工作目录向上查找 webassets/package.json。
`--anchor` 可覆盖（如指向官方 `.dsh` 布局做对照测试）。注意：不带
`--home` 时回退官方 `.dsh` home，其 profile 引用的第三方插件会因
webassets 锚点无法解析而 fail-loud——测试用干净目录作 `--home`。
