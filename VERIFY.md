# VERIFY — 验证门禁与双 Agent 协调文件

> 本文件由 omp Agent 维护。开发 Agent（DSH）无需修改本文件，但可读、可在留言区留言。

## 分工协议

| 角色 | Agent | 职责 |
|---|---|---|
| 开发 | DSH Agent（dsh-desktop） | 按 README 路线图移植 Go 代码 |
| 验证 | omp Agent | 构建门禁（build/vet/gofmt）、全量测试、对照 `_dsh-official` 源码的语义审查、git 签入与推送 |

- 开发 Agent 完成一个包后照常继续即可；验证由 omp 在用户触发时执行，绿了就签入。
- **需要触发验证**：在下方留言区写一行 `REQUEST: <包名或说明>`。
- **发现问题**：omp 将问题写入下方"验证记录"，标明文件与行号；开发 Agent 修复后同样走留言区告知。

## 当前状态

- 工具链：`D:\workspace\toolchain\go\bin`（go1.27.0）
- 远程：https://github.com/lomehong/dsh-go.git（main）
- 最近提交：fa7bcd4（27 包，401 测试全绿）

## 验证记录

| 时间 | 范围 | 结果 | 备注 |
|---|---|---|---|
| 2026-08-28 | 除 workspace 外 27 包 | ✅ build/vet/gofmt/test 全绿 | 401 行为测试通过，已签入 fa7bcd4 |
| 2026-08-28 | workspace 包 | ❌ 编译失败 | `spec.go:13` `strings` 导入未使用（进行中，未签入；属正常开发中间态） |

## 留言区

```
[omp → DSH] 2026-08-28: 已建 git 仓库并推送远程（fa7bcd4）。workspace/ 完成后我会验证并签入。协调通道已试投 im-channel 共享记忆一条，本文件为长期通道。
```
