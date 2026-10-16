# RelayNet 构建脚本 (Windows PowerShell)
# 用法: 在项目根目录执行  .\build.ps1
# 产物: bin\relayserver.exe  bin\relayclient.exe

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root
$bin = Join-Path $root "bin"
if (-not (Test-Path $bin)) { New-Item -ItemType Directory -Path $bin | Out-Null }

# CGO is required for audio (malgo) and WGC screen capture.
# Allow an explicit MinGW location, otherwise use gcc on PATH or a WinLibs
# winget installation under the current user's profile.
if ($env:RELAYNET_GCC_DIR) {
    $env:PATH = "$env:RELAYNET_GCC_DIR;$env:PATH"
} elseif (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
    $packages = Join-Path $env:LOCALAPPDATA "Microsoft\WinGet\Packages"
    $gcc = Get-ChildItem -Path $packages -Directory -Filter "BrechtSanders.WinLibs.POSIX.UCRT*" -ErrorAction SilentlyContinue |
        ForEach-Object { Join-Path $_.FullName "mingw64\bin" } |
        Where-Object { Test-Path (Join-Path $_ "gcc.exe") } |
        Select-Object -First 1
    if ($gcc) { $env:PATH = "$gcc;$env:PATH" }
}
if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
    throw "MinGW GCC not found. Add gcc to PATH or set RELAYNET_GCC_DIR."
}
$env:CGO_ENABLED = "1"

Write-Host "==> building relayserver.exe" -ForegroundColor Cyan
go build -o (Join-Path $bin "relayserver.exe") .\cmd\relayserver
if ($LASTEXITCODE -ne 0) { throw "build server failed" }

Write-Host "==> building relayclient.exe" -ForegroundColor Cyan
. (Join-Path $root "prepare-client-build.ps1")
go build -modfile $clientModFile -ldflags "-H windowsgui" -o (Join-Path $bin "relayclient.exe") .\cmd\relayclient
if ($LASTEXITCODE -ne 0) { throw "build client failed" }

Write-Host "==> done. binaries in $bin" -ForegroundColor Green
