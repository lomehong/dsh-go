# sync-webassets.ps1 — regenerate dsh-go's bundled web frontend from the
# official deepseek-harness source tree.
#
# The frontend stays TypeScript (Owner decision): apps/web + every package
# declaring dsh.client are built from the OFFICIAL SOURCE at the tag aligned
# with the Go backend, and the build products are staged into webassets/ in
# the anchor layout the Go loader expects:
#
#   webassets/
#     package.json                       (anchor marker)
#     node_modules/@deepseek-ai/
#       <pkg>/package.json               (dsh.client declaration source)
#       <pkg>/cordis.patch.yml           (bundle rows for the loader)
#       <pkg>/lib/client.js              (client half for the boot graph)
#       dsh-frontend/dist/               (vite shell, minus source maps)
#
# Usage:
#   pwsh scripts/sync-webassets.ps1 -Monorepo E:\code\nodejs\deepseek-harness
#
# Tag alignment: build from the same upstream tag the Go port tracks
# (dsh-v0.1.2-alpha.4 as of r96). Verify with:
#   git -C <monorepo> describe --tags
param(
    [Parameter(Mandatory = $true)]
    [string]$Monorepo
)

$ErrorActionPreference = "Stop"
# The script lives in <repo>/scripts; the repo root is its parent. Manual
# runs from elsewhere fall back to the current directory.
$repo = if ($PSScriptRoot) { Split-Path (Split-Path $PSScriptRoot) } else { (Get-Location).Path }
$webassets = Join-Path $repo "webassets"
$srcPkgs = Join-Path $Monorepo "packages"

if (-not (Test-Path (Join-Path $Monorepo "package.json"))) {
    throw "monorepo not found at $Monorepo"
}

function Build-PkgTsdown([string]$pkgDir) {
    pushd $pkgPath
    & pnpm exec tsdown 2>&1 | Out-Null
    $code = $LASTEXITCODE
    popd
    return $code
}

# --- 1. build the web dist (vite) -----------------------------------------
$webApp = Join-Path $Monorepo "apps\web"
pushd (Join-Path $Monorepo "packages\experimental\webworker-runtime")
& pnpm exec tsdown 2>&1 | Select-Object -Last 1
popd
pushd (Join-Path $Monorepo "packages\client\store")
& node --max-old-space-size=4096 "$Monorepo\node_modules\typescript\bin\tsc" -b tsconfig.client.json 2>&1 | Out-Null
& pnpm exec tsdown 2>&1 | Out-Null
if ($LASTEXITCODE -ne 0) { throw "client-store build failed" }
popd

pushd $webApp
& pnpm exec vite build 2>&1 | Select-Object -Last 3
if ($LASTEXITCODE -ne 0) { throw "vite build failed" }
popd

# --- 2. stage webassets ----------------------------------------------------
$srcRoot = Join-Path $Monorepo "apps\web\dist"
$dstFront = Join-Path $webassets "node_modules\@deepseek-ai\dsh-frontend"
if (Test-Path (Join-Path $webassets "node_modules")) {
    Remove-Item -Recurse -Force (Join-Path $webassets "node_modules")
}
New-Item -ItemType Directory -Force -Path $dstFront | Out-Null
# apps/web 的 vite outDir 即 dist（assets/index.html 直接在 dist 下），
# 直接复制该目录本身到 dsh-frontend/dist。
Copy-Item $srcRoot (Join-Path $dstFront "dist") -Recurse -Force
Get-ChildItem (Join-Path $dstFront "dist") -Recurse -File -Filter "*.map" | Remove-Item -Force
$frontPkg = Get-Content (Join-Path $Monorepo "apps\web\package.json") -Raw
Set-Content (Join-Path $dstFront "package.json") -Value $frontPkg -NoNewline

# --- 3. build + stage every client-half bundle -----------------------------
# Each package that declares dsh.client (platform web) ships lib/client.js
# (tsdown product). Build each, then copy package.json + cordis.patch.yml +
# lib/client.js into the anchor layout. Build failures are collected and
# reported at the end (known flaky: connection needs stale tsc products).
$pkgDirs = Get-ChildItem $srcPkgs -Recurse -Filter "package.json" -Depth 3 -ErrorAction SilentlyContinue |
    ForEach-Object {
        $j = Get-Content $_.FullName -Raw -ErrorAction SilentlyContinue | ConvertFrom-Json
        if ($j.dsh -and $j.dsh.client -and $j.dsh.client.platform -eq "web") {
            $_.DirectoryName
        }
    }
$built = 0
$failed = @()
foreach ($dir in $pkgDirs) {
    $name = (Get-Content (Join-Path $dir "package.json") -Raw | ConvertFrom-Json).name
    $clientTsdown = Test-Path (Join-Path $dir "tsdown.config.ts")
    $clientJson = Join-Path $dir "lib\client.js"
    if (-not (Test-Path $clientJson) -and -not $clientTsdown) {
        $failed += "$name (no tsdown config and no client.js)"
        continue
    }
    if ($clientTsdown) {
        pushd $dir
        & pnpm exec tsdown 2>&1 | Out-Null
        $code = $LASTEXITCODE
        popd
        if ($code -ne 0) {
            $failed += "$name (tsdown exit $code)"
            continue
        }
    }
    $short = $name -replace '^@deepseek-ai/', ''
    $dstPkg = Join-Path $webassets "node_modules\@deepseek-ai\$short"
    New-Item -ItemType Directory -Force -Path (Join-Path $dstPkg "lib") | Out-Null
    Copy-Item (Join-Path $dir "package.json") (Join-Path $dstPkg "package.json") -Force
    Copy-Item (Join-Path $dir "cordis.patch.yml") (Join-Path $dstPkg "cordis.patch.yml") -Force -ErrorAction SilentlyContinue
    Copy-Item (Join-Path $dir "lib\client.js") (Join-Path $dstPkg "lib\client.js") -Force
    $mapSrc = Join-Path $dir "lib\client.js.map"
    # source maps are skipped: the Go boot composer builds identity maps
    $built++
}
Write-Host "client bundles staged: $built; failed: $($failed.Count)"
if ($failed.Count -gt 0) { $failed | ForEach-Object { Write-Host "  FAIL: $_" } }
Write-Host "webassets sync complete: $webassets"
