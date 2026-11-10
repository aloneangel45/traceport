# ==============================================================================
# TracePort - Universal Installer (Windows PowerShell)
# Repository: https://github.com/aloneangel45/traceport
# ==============================================================================

$ErrorActionPreference = "Stop"

$Repo = "aloneangel45/traceport"
$BinaryName = "traceport.exe"
$InstallDir = Join-Path $env:USERPROFILE ".local\bin"

Write-Host "[traceport] initiating Windows installer"

# 1. Detect Architecture
$Arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64" -or $env:PROCESSOR_ARCHITEW6432 -eq "ARM64") {
    $Arch = "arm64"
}
Write-Host "[traceport] detected platform: windows/$Arch"

# 2. Resolve Latest Release
$Tag = "v1.1.0"
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $ReleaseInfo = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing -TimeoutSec 10
    if ($ReleaseInfo.tag_name) {
        $Tag = $ReleaseInfo.tag_name
    }
} catch {
    Write-Host "[warn] GitHub API unavailable; falling back to release $Tag"
}

Write-Host "[traceport] target release: $Tag"

# 3. Download Release Asset and checksums.txt
$ArchiveName = "traceport-windows-$Arch.zip"
$BaseUrl = "https://github.com/$Repo/releases/download/$Tag"
$DownloadUrl = "$BaseUrl/$ArchiveName"
$ChecksumsUrl = "$BaseUrl/checksums.txt"

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
    $ZipPath = Join-Path $TempDir $ArchiveName
    $ChecksumsPath = Join-Path $TempDir "checksums.txt"

    Write-Host "[traceport] downloading $DownloadUrl"
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $ZipPath -UseBasicParsing

    Write-Host "[traceport] downloading $ChecksumsUrl"
    Invoke-WebRequest -Uri $ChecksumsUrl -OutFile $ChecksumsPath -UseBasicParsing

    # 4. Verify SHA-256 Digest
    $ExpectedHash = $null
    foreach ($Line in (Get-Content -Path $ChecksumsPath)) {
        $Parts = ($Line -split "\s+") | Where-Object { $_ -ne "" }
        if ($Parts.Count -ge 2) {
            $FileField = $Parts[1].TrimStart("*")
            if ($FileField -eq $ArchiveName) {
                $ExpectedHash = $Parts[0].ToLowerInvariant()
                break
            }
        }
    }

    if (-not $ExpectedHash) {
        throw "Archive $ArchiveName not found in checksums.txt"
    }

    $ActualHash = (Get-FileHash -Path $ZipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($ActualHash -ne $ExpectedHash) {
        throw "SHA-256 checksum mismatch for ${ArchiveName}: expected $ExpectedHash, got $ActualHash"
    }
    Write-Host "[ok] verified SHA-256 digest: $ActualHash"

    # 5. Extract and Install
    Write-Host "[traceport] extracting archive"
    Expand-Archive -Path $ZipPath -DestinationPath $TempDir -Force

    $ExtractedBinary = Get-ChildItem -Path $TempDir -Filter "*.exe" -Recurse | Select-Object -First 1
    if (-not $ExtractedBinary) {
        throw "Extraction failed: executable not found in archive"
    }

    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    $DestinationFile = Join-Path $InstallDir $BinaryName
    Copy-Item -Path $ExtractedBinary.FullName -Destination $DestinationFile -Force
} finally {
    Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}

# 6. Ensure PATH is updated in User Environment
$UserPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if (-not $UserPath) {
    $UserPath = ""
}
$PathEntries = $UserPath -split ";" | Where-Object { $_ -ne "" }
if ($PathEntries -notcontains $InstallDir) {
    Write-Host "[traceport] adding $InstallDir to user PATH"
    $NewPath = if ($UserPath) { "$InstallDir;$UserPath" } else { $InstallDir }
    [Environment]::SetEnvironmentVariable("PATH", $NewPath, "User")
    $env:PATH = "$InstallDir;$env:PATH"
}

Write-Host "[ok] installed $DestinationFile"
Write-Host ""
Write-Host "Quick Start:"
Write-Host "  traceport --help"
Write-Host "  traceport record -- <cmd>"
Write-Host "  traceport sessions"
Write-Host "  traceport rewind <session_id> --step <N>"
Write-Host ""
