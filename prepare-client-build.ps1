# Fyne 2.8.1 draws at a fixed 60 Hz. Keep its original module cache intact,
# and build this client against a local copy with a 120 Hz drawing loop.
$ErrorActionPreference = "Stop"
$projectRoot = $PSScriptRoot
$fyneModule = go list -m -json fyne.io/fyne/v2 | ConvertFrom-Json
if ($LASTEXITCODE -ne 0) { throw "Cannot locate the Fyne module." }
if ($fyneModule.Version -ne "v2.8.1") { throw "Review the 120 Hz patch for this Fyne version first." }
$clientBuildRoot = Join-Path $projectRoot ".build"
$patchedFyneRoot = Join-Path $clientBuildRoot "fyne-$($fyneModule.Version)"
if (-not (Test-Path (Join-Path $patchedFyneRoot "go.mod"))) {
    New-Item -ItemType Directory -Path $patchedFyneRoot -Force | Out-Null
    Get-ChildItem -LiteralPath $fyneModule.Dir -Force |
        Copy-Item -Destination $patchedFyneRoot -Recurse -Force
}
$loopRelativePath = "internal/driver/glfw/loop.go"
$upstreamLoop = [IO.File]::ReadAllText((Join-Path $fyneModule.Dir $loopRelativePath))
$oldPeriod = "eventTick := time.NewTicker(time.Second / 60)"
if (($upstreamLoop.Split([string[]]@($oldPeriod), [StringSplitOptions]::None)).Length -ne 2) {
    throw "Fyne drawing loop changed; review the 120 Hz patch."
}
$patchedLoopPath = Join-Path $patchedFyneRoot $loopRelativePath
(Get-Item -LiteralPath $patchedLoopPath).IsReadOnly = $false
$utf8 = New-Object Text.UTF8Encoding($false)
[IO.File]::WriteAllText($patchedLoopPath, $upstreamLoop.Replace($oldPeriod, "eventTick := time.NewTicker(time.Second / 120)"), $utf8)
$clientModFile = Join-Path $clientBuildRoot "relaynet.mod"
$replacement = $patchedFyneRoot.Replace('\', '/')
$moduleText = [IO.File]::ReadAllText((Join-Path $projectRoot "go.mod"))
[IO.File]::WriteAllText($clientModFile, $moduleText + "`nreplace fyne.io/fyne/v2 => `"$replacement`"`n", $utf8)
Copy-Item -LiteralPath (Join-Path $projectRoot "go.sum") -Destination (Join-Path $clientBuildRoot "relaynet.sum") -Force
