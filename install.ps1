# ==============================================================================
# TracePort - Universal Installer (Windows PowerShell)
# Repository: https://github.com/aloneangel45/traceport
# ==============================================================================

$ErrorActionPreference = "Stop"

# Ensure built-in PowerShell module path is present when invoked in stripped CI/container shells,
# while still using pure .NET APIs throughout so zero external modules are required.
if ($PSHome) {
    $BuiltInModules = [System.IO.Path]::Combine($PSHome, "Modules")
    if ([string]::IsNullOrWhiteSpace($env:PSModulePath)) {
        $env:PSModulePath = $BuiltInModules
    }
}

$Repo = "aloneangel45/traceport"
$BinaryName = "traceport.exe"
$InstallDir = if ($env:TRACEPORT_INSTALL_DIR) {
    $env:TRACEPORT_INSTALL_DIR
} else {
    [System.IO.Path]::Combine([System.IO.Path]::Combine($env:USERPROFILE, ".local"), "bin")
}

function Invoke-TracePortDownload([string]$Url, [string]$OutFile) {
    $Client = [System.Net.WebClient]::new()
    try {
        $Client.Headers.Add("User-Agent", "traceport-installer")
        $Client.DownloadFile($Url, $OutFile)
    } finally {
        $Client.Dispose()
    }
}

function Invoke-TracePortDownloadString([string]$Url) {
    $Client = [System.Net.WebClient]::new()
    try {
        $Client.Headers.Add("User-Agent", "traceport-installer")
        return $Client.DownloadString($Url)
    } finally {
        $Client.Dispose()
    }
}

[Console]::WriteLine("[traceport] initiating Windows installer")

# 1. Detect Architecture
$Arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64" -or $env:PROCESSOR_ARCHITEW6432 -eq "ARM64") {
    $Arch = "arm64"
}
[Console]::WriteLine("[traceport] detected platform: windows/$Arch")

# Enable TLS 1.2 (3072) while preserving any newer enabled protocols (e.g. TLS 1.3)
try {
    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
} catch {
}

# 2. Resolve Target Release & Base URL
$Tag = if ($env:TRACEPORT_VERSION) { $env:TRACEPORT_VERSION } else { "" }
$BaseUrl = if ($env:TRACEPORT_RELEASE_BASE_URL) { $env:TRACEPORT_RELEASE_BASE_URL.TrimEnd("/") } else { "" }

if (-not $BaseUrl) {
    if (-not $Tag) {
        $Tag = "v1.2.1"
        try {
            $ReleaseJson = Invoke-TracePortDownloadString "https://api.github.com/repos/$Repo/releases/latest"
            $Match = [System.Text.RegularExpressions.Regex]::Match($ReleaseJson, '"tag_name"\s*:\s*"([^"]+)"')
            if ($Match.Success -and $Match.Groups[1].Value) {
                $Tag = $Match.Groups[1].Value
            }
        } catch {
            [Console]::WriteLine("[warn] GitHub API unavailable; falling back to release $Tag")
        }
    }
    [Console]::WriteLine("[traceport] target release: $Tag")
    $BaseUrl = "https://github.com/$Repo/releases/download/$Tag"
} else {
    [Console]::WriteLine("[traceport] using custom release base URL: $BaseUrl")
}

# 3. Download Release Asset and checksums.txt
$ArchiveName = "traceport-windows-$Arch.zip"
$DownloadUrl = "$BaseUrl/$ArchiveName"
$ChecksumsUrl = "$BaseUrl/checksums.txt"

$TempDir = [System.IO.Path]::Combine([System.IO.Path]::GetTempPath(), [System.Guid]::NewGuid().ToString())
$null = [System.IO.Directory]::CreateDirectory($TempDir)

try {
    $ZipPath = [System.IO.Path]::Combine($TempDir, $ArchiveName)
    $ChecksumsPath = [System.IO.Path]::Combine($TempDir, "checksums.txt")

    [Console]::WriteLine("[traceport] downloading $DownloadUrl")
    Invoke-TracePortDownload $DownloadUrl $ZipPath

    [Console]::WriteLine("[traceport] downloading $ChecksumsUrl")
    Invoke-TracePortDownload $ChecksumsUrl $ChecksumsPath

    # 4. Verify SHA-256 Digest (pure .NET for restricted PSModulePath compatibility)
    $ExpectedHash = $null
    foreach ($Line in [System.IO.File]::ReadAllLines($ChecksumsPath)) {
        $Trimmed = $Line.Trim()
        if ($Trimmed.Length -eq 0) {
            continue
        }
        $Parts = $Trimmed -split "\s+"
        if ($Parts.Length -ge 2) {
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

    $Sha256 = [System.Security.Cryptography.SHA256]::Create()
    try {
        $FileStream = [System.IO.File]::OpenRead($ZipPath)
        try {
            $HashBytes = $Sha256.ComputeHash($FileStream)
        } finally {
            $FileStream.Dispose()
        }
    } finally {
        $Sha256.Dispose()
    }
    $ActualHash = [System.BitConverter]::ToString($HashBytes).Replace("-", "").ToLowerInvariant()
    if ($ActualHash -ne $ExpectedHash) {
        throw "SHA-256 checksum mismatch for ${ArchiveName}: expected $ExpectedHash, got $ActualHash"
    }
    [Console]::WriteLine("[ok] verified SHA-256 digest: $ActualHash")

    # 5. Extract and Install (pure .NET ZipFile without Expand-Archive module dependency)
    [Console]::WriteLine("[traceport] extracting archive")
    if (-not ("System.IO.Compression.ZipFile" -as [type])) {
        try {
            Add-Type -AssemblyName System.IO.Compression.FileSystem
        } catch {
            $null = [System.Reflection.Assembly]::LoadWithPartialName("System.IO.Compression.FileSystem")
        }
    }
    $ExtractDir = [System.IO.Path]::Combine($TempDir, "extracted")
    [System.IO.Compression.ZipFile]::ExtractToDirectory($ZipPath, $ExtractDir)

    $CandidateDirect = [System.IO.Path]::Combine($ExtractDir, $BinaryName)
    $CandidateArch = [System.IO.Path]::Combine($ExtractDir, "traceport-windows-$Arch.exe")
    $ExtractedBinaryPath = $null
    if ([System.IO.File]::Exists($CandidateDirect)) {
        $ExtractedBinaryPath = $CandidateDirect
    } elseif ([System.IO.File]::Exists($CandidateArch)) {
        $ExtractedBinaryPath = $CandidateArch
    } else {
        $FoundExes = [System.IO.Directory]::GetFiles($ExtractDir, "*.exe", [System.IO.SearchOption]::AllDirectories)
        foreach ($Candidate in $FoundExes) {
            $LeafName = [System.IO.Path]::GetFileName($Candidate)
            if ([string]::Equals($LeafName, $BinaryName, [System.StringComparison]::OrdinalIgnoreCase) -or
                [string]::Equals($LeafName, "traceport-windows-$Arch.exe", [System.StringComparison]::OrdinalIgnoreCase)) {
                $ExtractedBinaryPath = $Candidate
                break
            }
        }
        if (-not $ExtractedBinaryPath -and $FoundExes.Length -gt 0) {
            $ExtractedBinaryPath = $FoundExes[0]
        }
    }

    if (-not $ExtractedBinaryPath) {
        throw "Extraction failed: executable not found in archive"
    }

    $null = [System.IO.Directory]::CreateDirectory($InstallDir)

    $DestinationFile = [System.IO.Path]::Combine($InstallDir, $BinaryName)
    if ([System.IO.File]::Exists($DestinationFile)) {
        try {
            [System.IO.File]::SetAttributes($DestinationFile, [System.IO.FileAttributes]::Normal)
        } catch {
        }
    }
    [System.IO.File]::Copy($ExtractedBinaryPath, $DestinationFile, $true)
} finally {
    try {
        if ([System.IO.Directory]::Exists($TempDir)) {
            [System.IO.Directory]::Delete($TempDir, $true)
        }
    } catch {
        # Ignore best-effort temporary directory cleanup errors
    }
}

# 6. Ensure PATH is updated in User Environment
if ($env:TRACEPORT_SKIP_PATH_UPDATE -ne "1") {
    $UserPath = [Environment]::GetEnvironmentVariable("PATH", "User")
    if (-not $UserPath) {
        $UserPath = ""
    }
    $AlreadyPresent = $false
    foreach ($Entry in $UserPath.Split([char[]]@(';'), [System.StringSplitOptions]::RemoveEmptyEntries)) {
        if ([string]::Equals($Entry.TrimEnd('\'), $InstallDir.TrimEnd('\'), [System.StringComparison]::OrdinalIgnoreCase)) {
            $AlreadyPresent = $true
            break
        }
    }
    if (-not $AlreadyPresent) {
        [Console]::WriteLine("[traceport] adding $InstallDir to user PATH")
        $NewPath = if ($UserPath) { "$InstallDir;$UserPath" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable("PATH", $NewPath, "User")
        $env:PATH = "$InstallDir;$env:PATH"
    }
}

[Console]::WriteLine("[ok] installed $DestinationFile")
[Console]::WriteLine("")
[Console]::WriteLine("Quick Start:")
[Console]::WriteLine("  traceport --help")
[Console]::WriteLine("  traceport record -- <cmd>")
[Console]::WriteLine("  traceport sessions")
[Console]::WriteLine("  traceport rewind <session_id> --step <N>")
[Console]::WriteLine("")
