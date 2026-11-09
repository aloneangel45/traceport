# ==============================================================================
# TracePort - Official Universal Installer (Windows PowerShell)
# AI Agent Flight Recorder & Time-Travel Debugger
# Repository: https://github.com/aloneangel45/traceport
# ==============================================================================

$ErrorActionPreference = "Stop"

$Repo = "aloneangel45/traceport"
$BinaryName = "traceport.exe"
$InstallDir = Join-Path $env:USERPROFILE ".local\bin"

Write-Host ">>> [INFO] Initiating TracePort Windows installer..." -ForegroundColor Cyan

# 1. Detect Architecture
$Arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") {
    $Arch = "arm64"
}
Write-Host ">>> [INFO] Detected architecture: Windows/$Arch" -ForegroundColor Gray

# 2. Resolve Latest Release
Write-Host ">>> [INFO] Resolving latest release version from GitHub..." -ForegroundColor Gray
$Tag = "v1.0.0"
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $ReleaseInfo = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing -TimeoutSec 10
    if ($ReleaseInfo.tag_name) {
        $Tag = $ReleaseInfo.tag_name
    }
} catch {
    Write-Host ">>> [WARN] Using default release fallback: $Tag" -ForegroundColor Yellow
}

Write-Host ">>> [INFO] Target release: $Tag" -ForegroundColor Green

# 3. Download Release Asset
$ArchiveName = "traceport-windows-amd64.zip"
$DownloadUrl = "https://github.com/$Repo/releases/download/$Tag/$ArchiveName"
$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null
$ZipPath = Join-Path $TempDir $ArchiveName

Write-Host ">>> [INFO] Downloading $DownloadUrl..." -ForegroundColor Gray
try {
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $ZipPath -UseBasicParsing
} catch {
    Write-Error "Failed to download TracePort package from $DownloadUrl"
    exit 1
}

# 4. Extract and Install
Write-Host ">>> [INFO] Extracting archive..." -ForegroundColor Gray
Expand-Archive -Path $ZipPath -DestinationPath $TempDir -Force

$ExtractedBinary = Get-ChildItem -Path $TempDir -Filter "*.exe" -Recurse | Select-Object -First 1
if (-not $ExtractedBinary) {
    Write-Error "Extraction failed: Executable not found in archive."
    exit 1
}

if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

$DestinationFile = Join-Path $InstallDir $BinaryName
Copy-Item -Path $ExtractedBinary.FullName -Destination $DestinationFile -Force

# 5. Clean up
Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue

# 6. Ensure PATH is updated in User Environment
$UserPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($UserPath -notlike "*$InstallDir*") {
    Write-Host ">>> [INFO] Adding $InstallDir to User PATH..." -ForegroundColor Cyan
    $NewPath = "$InstallDir;$UserPath"
    [Environment]::SetEnvironmentVariable("PATH", $NewPath, "User")
    $env:PATH = "$InstallDir;$env:PATH"
}

Write-Host ""
Write-Host "================================================================" -ForegroundColor Green
Write-Host "  TracePort installed successfully to $DestinationFile" -ForegroundColor Green
Write-Host "================================================================" -ForegroundColor Green
Write-Host ""
Write-Host "  Quick Start:" -ForegroundColor White
Write-Host "    traceport --help                                      # View options" -ForegroundColor Yellow
Write-Host "    traceport record -- aider --model claude-3-7-sonnet   # Record session" -ForegroundColor Yellow
Write-Host "    traceport sessions                                    # List runs" -ForegroundColor Yellow
Write-Host "    traceport rewind <id> --step 3                        # Rollback" -ForegroundColor Yellow
Write-Host ""
Write-Host "  Documentation: https://github.com/$Repo" -ForegroundColor Gray
Write-Host ""
