#Requires -Version 5.0
<#
.SYNOPSIS
  Builds libtokenizers.a for Windows from daulet/tokenizers source.

.DESCRIPTION
  daulet/tokenizers does not publish Windows prebuilts, so we build the
  Rust static library locally. Output is placed at libs\windows\libtokenizers.a
  where our cgo LDFLAGS expects it.

  One-time setup (~10-15 min on first run, mostly cargo dependency compilation).
  Subsequent runs are instant if libtokenizers.a already matches the version.

.PREREQUISITES
  - rustup installed (https://rustup.rs)
  - git in PATH
  - w64devkit gcc in PATH (for mingw-gnu target linkage compatibility with
    our final Go binary built via w64devkit)

.NOTES
  The Rust target is x86_64-pc-windows-gnu (NOT msvc) because our final
  Go binary is built with w64devkit gcc, and gnu/msvc static libs are not
  ABI-compatible.
#>

param(
    [string]$OutputPath = "$PSScriptRoot\..\libs\windows\libtokenizers.a",
    [switch]$Force
)

$ErrorActionPreference = 'Stop'

$TokenizersCommit = 'f678a7768d5479d9d5a5161c4fc45c8a5ba46146'
$RustToolchain = '1.95.0-x86_64-pc-windows-gnu'
$RustTarget = 'x86_64-pc-windows-gnu'
$BuildRevision = "$TokenizersCommit rust=$RustToolchain target=$RustTarget"

$repoRoot = Resolve-Path "$PSScriptRoot\.."
$dstLib = [System.IO.Path]::GetFullPath($OutputPath)
$libsDir = Split-Path -Parent $dstLib
$revisionPath = "$dstLib.revision"
$targetDir = $env:CARGO_TARGET_DIR
if ([string]::IsNullOrWhiteSpace($targetDir)) {
    $targetDir = Join-Path $repoRoot '.task\tokenizers-target\windows-amd64'
}
$targetDir = [System.IO.Path]::GetFullPath($targetDir)

if (-not $Force -and (Test-Path -LiteralPath $dstLib) -and (Test-Path -LiteralPath $revisionPath)) {
    $builtRevision = (Get-Content -LiteralPath $revisionPath -Raw).Trim()
    if ($builtRevision -eq $BuildRevision) {
        Write-Host "libtokenizers.a already matches $BuildRevision"
        exit 0
    }
}

if (-not (Get-Command rustup -ErrorAction SilentlyContinue)) {
    Write-Host ''
    Write-Host 'rustup is not installed.' -ForegroundColor Red
    Write-Host 'Install from https://rustup.rs (one-time, ~5 min).'
    Write-Host 'After install, re-run this script.'
    exit 1
}

if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
    Write-Host 'git is not installed or not in PATH.' -ForegroundColor Red
    exit 1
}

if (-not (Get-Command cargo -ErrorAction SilentlyContinue)) {
    Write-Host 'cargo is not installed or not in PATH.' -ForegroundColor Red
    exit 1
}

$gcc = Get-Command gcc -ErrorAction SilentlyContinue
if (-not $gcc) {
    Write-Host 'gcc is not installed or not in PATH (w64devkit/MinGW is required).' -ForegroundColor Red
    exit 1
}

$installedToolchains = rustup toolchain list
if ($LASTEXITCODE -ne 0) { throw "rustup toolchain list failed" }
# rustup prints "<toolchain> (active, default)", so the name is the first field
# and never has a trailing dash. Matching one reinstalled the toolchain on every
# build.
if (-not ($installedToolchains | Where-Object { ($_ -split '\s+')[0] -eq $RustToolchain })) {
    Write-Host "Installing Rust $RustToolchain (one-time)..." -ForegroundColor Cyan
    rustup toolchain install $RustToolchain --profile minimal
    if ($LASTEXITCODE -ne 0) { throw "rustup toolchain install failed" }
}

$installedTargets = rustup target list --installed --toolchain $RustToolchain
if ($LASTEXITCODE -ne 0) { throw "rustup target list failed" }
if ($installedTargets -notcontains $RustTarget) {
    Write-Host "Installing Rust target $RustTarget (one-time)..." -ForegroundColor Cyan
    rustup target add $RustTarget --toolchain $RustToolchain
    if ($LASTEXITCODE -ne 0) { throw "rustup target add failed" }
}

$workDir = Join-Path ([System.IO.Path]::GetTempPath()) ("contextmaxxer-tokenizers-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $workDir | Out-Null

Write-Host "Fetching daulet/tokenizers $TokenizersCommit ..." -ForegroundColor Cyan
git -C $workDir init --quiet
if ($LASTEXITCODE -ne 0) { throw "git init failed" }
git -C $workDir remote add origin https://github.com/daulet/tokenizers
if ($LASTEXITCODE -ne 0) { throw "git remote add failed" }
git -C $workDir fetch --depth 1 origin $TokenizersCommit
if ($LASTEXITCODE -ne 0) { throw "git fetch failed" }
git -C $workDir checkout --quiet --detach FETCH_HEAD
if ($LASTEXITCODE -ne 0) { throw "git checkout failed" }

Push-Location $workDir
try {
    Write-Host "Building libtokenizers.a (target=$RustTarget release)..." -ForegroundColor Cyan
    Write-Host '(first run downloads ~200 crates and may take 5-15 minutes)'
    $env:CARGO_TARGET_X86_64_PC_WINDOWS_GNU_LINKER = $gcc.Source
    $env:CARGO_TARGET_DIR = $targetDir
    cargo "+$RustToolchain" build --release --target $RustTarget
    if ($LASTEXITCODE -ne 0) { throw "cargo build failed" }
} finally {
    Pop-Location
}

$srcLib = Join-Path $targetDir "$RustTarget\release\libtokenizers_ffi.a"
if (-not (Test-Path $srcLib)) {
    Write-Host "Build succeeded but $srcLib not found." -ForegroundColor Red
    Write-Host 'Inspect target dir manually:'
    Get-ChildItem -Recurse (Join-Path $targetDir "$RustTarget\release") -Filter '*.a' | Select-Object FullName
    exit 1
}

New-Item -ItemType Directory -Force -Path $libsDir | Out-Null
Copy-Item -Force $srcLib $dstLib
[System.IO.File]::WriteAllText(
    $revisionPath,
    $BuildRevision + [Environment]::NewLine,
    (New-Object System.Text.UTF8Encoding($false))
)
$sizeMB = (Get-Item $dstLib).Length / 1MB

Write-Host ''
Write-Host ("libtokenizers.a built at {0} ({1:N1} MB)" -f $dstLib, $sizeMB) -ForegroundColor Green
Write-Host 'Now run: task build:windows'

$resolvedWorkDir = [System.IO.Path]::GetFullPath($workDir)
$resolvedTempRoot = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
if (-not $resolvedWorkDir.StartsWith($resolvedTempRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Refusing to clean build directory outside the system temp root: $resolvedWorkDir"
}
Remove-Item -LiteralPath $resolvedWorkDir -Recurse -Force
