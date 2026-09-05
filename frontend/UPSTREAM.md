# Upstream source pin — web frontend fork

This directory is a source fork of the official deepseek-harness web surface:
the Go host tracks the upstream web implementation at one pinned commit, and
the composition guard (guard v5) fails when this pin drifts from the official
clone the sync ran against.

- Upstream commit: d347e703908d0406b7a7ef80e3a0e594d86b2215
- Upstream tag: dsh-v0.1.3-alpha.1
- Synced at: 2026-09-05 10:41:28 +08:00
- Closure packages: 86 @deepseek-ai packages + apps/web
- Seed: 72 web-app client bundles + dsh-frontend + dsh-web-app

## Regenerate

    pwsh scripts/sync-frontend.ps1 -Monorepo E:\code\nodejs\deepseek-harness

The sync copies apps/web and every @deepseek-ai package in the web-app
dependency closure (source only: lib/, dist/, node_modules stripped), then
rebuilds this manifest. Commit the result together so the guard can compare
frontend/UPSTREAM.md against the live clone.
