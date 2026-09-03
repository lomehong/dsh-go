# sync-frontend.ps1 — fork the upstream web frontend source closure into frontend/.
#
# Owner-approved fork strategy (方案 A): copy the official deepseek-harness
# source the Go host's web surface tracks — apps/web, every dsh.client web
# package, and their @deepseek-ai/* dependency closure — into this repo's
# frontend/ directory, preserving the upstream relative layout. UPSTREAM.md
# pins the source commit/tag so the composition guard can fail on drift
# (guard v5).
#
# Usage:
#   pwsh scripts/sync-frontend.ps1 -Monorepo E:\code\nodejs\deepseek-harness
param(
    [Parameter(Mandatory = $true)]
    [string]$Monorepo
)

$ErrorActionPreference = "Stop"
# The script lives in <repo>/scripts; the repo root is its parent (single
# Split-Path — a double split descends under the parent workspace).
$repo = if ($PSScriptRoot) { Split-Path $PSScriptRoot } else { (Get-Location).Path }
$frontend = Join-Path $repo "frontend"
$srcPkgs = Join-Path $Monorepo "packages"
$srcApps = Join-Path $Monorepo "apps"

if (-not (Test-Path (Join-Path $Monorepo "package.json"))) {
    throw "monorepo not found at $Monorepo"
}

# --- resolve the seed set ------------------------------------------------
# Seed = the web-app bundle's @deepseek-ai deps that ship a browser half
# (dsh.client), plus the web shell and the bundle packages themselves. The
# web-app bundle lives in the monorepo source tree (packages/bundle/web-app);
# resolve its manifest by name across packages/*, not node_modules (pnpm
# workspace layout does not hoist it under the root).
function Find-SourcePkg([string]$name) {
    foreach ($root in @((Join-Path $Monorepo "packages"), (Join-Path $Monorepo "apps"), (Join-Path $Monorepo "vendor"))) {
        foreach ($candidate in (Get-ChildItem $root -Recurse -Filter package.json -Depth 4 -ErrorAction SilentlyContinue)) {
            $j = Get-Content $candidate.FullName -Raw -ErrorAction SilentlyContinue | ConvertFrom-Json
            if ($j.name -eq $name) { return $candidate.DirectoryName }
        }
    }
    return $null
}

$webAppDir = Find-SourcePkg "@deepseek-ai/dsh-web-app"
if (-not $webAppDir) { throw "dsh-web-app not found under monorepo packages" }
$wa = Get-Content (Join-Path $webAppDir "package.json") -Raw | ConvertFrom-Json

# The @deepseek-ai/* dependency roots (where each scoped package's manifest
# lives): prefer the monorepo source tree, fall back to the installed layout.
$seed = @()
foreach ($d in $wa.dependencies.PSObject.Properties.Name) {
    if ($d -like '@deepseek-ai/*') {
        $srcCandidate = Find-SourcePkg $d
        if ($srcCandidate) { $seed += $d }
    }
}
# The web shell rides along under its real source name (apps/web declares
# @deepseek-ai/dsh-web-frontend; webassets' dsh-frontend is a staging alias).
$seed += '@deepseek-ai/dsh-web-frontend'
$seed += '@deepseek-ai/dsh-web-app'
$seed = $seed | Sort-Object -Unique

# --- BFS the @deepseek-ai closure from source package.json ----------------
function Get-PkgDir([string]$name) {
    foreach ($root in @($srcPkgs, $srcApps, (Join-Path $Monorepo "vendor"))) {
        foreach ($candidate in (Get-ChildItem $root -Recurse -Filter package.json -Depth 4 -ErrorAction SilentlyContinue)) {
            $j = Get-Content $candidate.FullName -Raw -ErrorAction SilentlyContinue | ConvertFrom-Json
            if ($j.name -eq $name) { return $candidate.DirectoryName }
        }
    }
    return $null
}
$closure = @{}
$queue = [System.Collections.Generic.Queue[string]]::new()
foreach ($s in $seed) { if (-not $closure.ContainsKey($s)) { $closure[$s] = $true; $queue.Enqueue($s) } }
while ($queue.Count -gt 0) {
    $p = $queue.Dequeue()
    $dir = Get-PkgDir $p
    if (-not $dir) { continue }
    $j = Get-Content (Join-Path $dir "package.json") -Raw | ConvertFrom-Json
    if ($j.dependencies) {
        foreach ($d in ($j.dependencies.PSObject.Properties.Name | Where-Object { $_ -like '@deepseek-ai/*' })) {
            if (-not $closure.ContainsKey($d)) { $closure[$d] = $true; $queue.Enqueue($d) }
        }
    }
}

# --- copy source closure into frontend/ -----------------------------------
# Dest dir name: strip @deepseek-ai/ scope -> node_modules/@deepseek-ai/<name>
$dstRoot = Join-Path $frontend "node_modules\@deepseek-ai"
New-Item -ItemType Directory -Force -Path $dstRoot | Out-Null
$copied = 0
$missing = @()
# robocopy excludes avoid recursing into pnpm's symlinked node_modules
# (an infinite nested-cordis loop) and drop build outputs at copy time.
$exclDirs = @("node_modules", "dist", ".turbo", "lib")
$robocopyArgs = @("/E", "/NFL", "/NDL", "/NJH", "/NJS", "/NP")
foreach ($d in $exclDirs) { $robocopyArgs += @("/XD", $d) }
foreach ($name in ($closure.Keys | Sort-Object)) {
    $dir = Get-PkgDir $name
    if (-not $dir) { $missing += $name; continue }
    $short = $name -replace '@deepseek-ai/',''
    $dst = Join-Path $dstRoot $short
    New-Item -ItemType Directory -Force -Path $dst | Out-Null
    & robocopy $dir $dst @robocopyArgs | Out-Null
    $copied++
}

# copy the web shell app source (apps/web) preserving relative structure;
# robocopy excludes node_modules to avoid the pnpm symlink loop.
$dstApp = Join-Path $frontend "apps\web"
New-Item -ItemType Directory -Force -Path $dstApp | Out-Null
& robocopy (Join-Path $srcApps "web") $dstApp @robocopyArgs | Out-Null

# --- write UPSTREAM.md manifest -------------------------------------------
$head = git -C $Monorepo rev-parse HEAD 2>$null
$tag = git -C $Monorepo describe --tags 2>$null
$stamp = Get-Date -Format "yyyy-MM-dd HH:mm:ss zzz"
$manifest = @"
# Upstream source pin — web frontend fork

This directory is a source fork of the official deepseek-harness web surface:
the Go host tracks the upstream web implementation at one pinned commit, and
the composition guard (guard v5) fails when this pin drifts from the official
clone the sync ran against.

- Upstream commit: $head
- Upstream tag: $tag
- Synced at: $stamp
- Closure packages: $($closure.Count) @deepseek-ai packages + apps/web
- Seed: $($seed.Count) web-app client bundles + dsh-frontend + dsh-web-app

## Regenerate

    pwsh scripts/sync-frontend.ps1 -Monorepo E:\code\nodejs\deepseek-harness

The sync copies apps/web and every @deepseek-ai package in the web-app
dependency closure (source only: lib/, dist/, node_modules stripped), then
rebuilds this manifest. Commit the result together so the guard can compare
frontend/UPSTREAM.md against the live clone.
"@
Set-Content (Join-Path $frontend "UPSTREAM.md") -Value $manifest -Encoding UTF8

Write-Host "frontend fork: $copied packages copied; $($missing.Count) unresolved"
if ($missing.Count -gt 0) { $missing | ForEach-Object { Write-Host "  MISSING: $_" } }
Write-Host "upstream: $head ($tag) @ $stamp"
Write-Host "frontend sync complete: $frontend"
