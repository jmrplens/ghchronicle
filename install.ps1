<#
.SYNOPSIS
  Puts the ghchronicle binary on this machine.

.DESCRIPTION
  The Windows half of install.sh, with the same promise: work out the
  architecture, resolve a version, download that archive and the release's
  checksum file, and refuse to install anything whose SHA-256 is not the one
  the release published. There is no switch to skip that. A script piped into
  a shell is the least inspectable way to install anything, so the one thing it
  must not do is trust what it just downloaded. When cosign 2.4.2 or newer is
  on PATH it also verifies that the checksum file came from the release
  workflow, and refuses when cosign does not confirm it.

  Written for Windows PowerShell 5.1 as well as PowerShell 7, because 5.1 is
  the one already on the machine: no null-coalescing, no ternaries, no
  three-argument Join-Path, and TLS 1.2 asked for explicitly, which 5.1 on an
  older Windows does not default to.

.EXAMPLE
  irm https://raw.githubusercontent.com/jmrplens/ghchronicle/main/install.ps1 | iex

.EXAMPLE
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/jmrplens/ghchronicle/main/install.ps1))) -Version 2.0.0
#>
[CmdletBinding()]
param(
  # The release to install. Empty takes the newest one.
  [string]$Version = $env:VERSION,
  # Where to put the binary. Empty picks a per-user directory.
  [string]$BinDir = $env:BIN_DIR,
  # Leave the user PATH alone. Without this the directory is added to it when
  # it is not there already, which is what makes "ghchronicle" work by name.
  [switch]$NoPathUpdate
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Repo = 'jmrplens/ghchronicle'
$Binary = 'ghchronicle'

# Overridable so the tests can point the whole thing at a local server, and so
# a mirror is possible at all.
$DownloadBase = $env:GHCHRONICLE_DOWNLOAD_BASE
if (-not $DownloadBase) { $DownloadBase = "https://github.com/$Repo/releases/download" }
$LatestUrl = $env:GHCHRONICLE_LATEST_URL
if (-not $LatestUrl) { $LatestUrl = "https://api.github.com/repos/$Repo/releases/latest" }

# The workflow that signs a release, as the certificate records it. The same
# two values install.sh passes, so the two installers accept exactly the same
# signer; a test holds them to each other.
$CosignIdentity = '^https://github\.com/jmrplens/ghchronicle/\.github/workflows/release\.yml@refs/tags/v'
$CosignIssuer = 'https://token.actions.githubusercontent.com'

# The oldest cosign that reads the bundle every release publishes. install.sh
# names the same one and says how it was found.
$CosignMinimum = [version]'2.4.2'

# 5.1 on Windows Server 2016 and older Windows 10 negotiates TLS 1.0 by
# default, which github.com has not accepted for years: without this the first
# download fails with a connection error that says nothing about why.
try {
  [Net.ServicePointManager]::SecurityProtocol =
    [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch {
  # PowerShell 7 on a platform where this type is not settable. It negotiates
  # TLS 1.2 or better on its own.
}

function Write-Step { param([string]$Message) Write-Host $Message }

function Get-Architecture {
  $arch = $env:PROCESSOR_ARCHITECTURE
  if (-not $arch) { $arch = '' }
  switch ($arch.ToUpperInvariant()) {
    'AMD64' { return 'amd64' }
    'ARM64' { return 'arm64' }
    'X86' {
      # A 32-bit PowerShell on a 64-bit machine reports x86. The 64-bit
      # archive is the right one, and the machine says so in another variable.
      if ($env:PROCESSOR_ARCHITEW6432 -and
          $env:PROCESSOR_ARCHITEW6432.ToUpperInvariant() -eq 'AMD64') { return 'amd64' }
      throw "no release is built for 32-bit x86"
    }
    default { throw "no release is built for $arch" }
  }
}

function Get-NewestVersion {
  try {
    $release = Invoke-RestMethod -Uri $LatestUrl -UseBasicParsing
  } catch {
    throw "could not read the newest version from $LatestUrl. Pass -Version, or take the archive from the releases page."
  }
  return ($release.tag_name -replace '^v', '')
}

function Assert-Checksum {
  param([string]$Directory, [string]$Archive)
  $checksums = Join-Path $Directory 'checksums.txt'
  # The name is matched as a whole field rather than as a substring, because
  # every archive's name is a prefix of its SBOM's: "..._windows_amd64.zip" is
  # the start of "..._windows_amd64.zip.spdx.json", and matching both checks a
  # file this script never downloaded.
  $wanted = $null
  foreach ($line in Get-Content -LiteralPath $checksums) {
    $fields = $line -split '\s+', 2
    if ($fields.Count -eq 2 -and $fields[1].Trim() -eq $Archive) { $wanted = $fields[0]; break }
  }
  if (-not $wanted) { throw "$Archive is not named in the release's checksums.txt" }

  $actual = (Get-FileHash -LiteralPath (Join-Path $Directory $Archive) -Algorithm SHA256).Hash
  if ($actual.ToLowerInvariant() -ne $wanted.ToLowerInvariant()) {
    throw "$Archive does not match the checksum the release published. Do not use it."
  }
}

# Assert-Signature runs only when cosign is already here, as install.sh's
# verify_signature does. The checksum above is what every install is held to;
# this answers the further question of whether the checksum file itself came
# from the release workflow, and it is worth doing when the tool is at hand
# rather than worth installing a tool for. Each way of not asking is said, in
# install.sh's words, so the checksum line is never taken for the whole check.
# A cosign too old to read the bundle is one of those ways, and a newer one
# that fails is refused with what it said, for install.sh's reasons.
function Assert-Signature {
  param([string]$Directory, [string]$Release)
  $cosign = Get-Command cosign -CommandType Application -ErrorAction SilentlyContinue |
    Select-Object -First 1
  if (-not $cosign) {
    Write-Step 'note: cosign is not on PATH, so only the checksum was verified'
    return
  }
  $bundle = Join-Path $Directory 'checksums.txt.sigstore.json'
  try {
    Invoke-WebRequest -Uri "$DownloadBase/v$Release/checksums.txt.sigstore.json" `
      -OutFile $bundle -UseBasicParsing
  } catch {
    Write-Step 'note: this release publishes no signature bundle, so only the checksum was verified'
    return
  }
  $have = Get-CosignVersion -Cosign $cosign.Source
  if ($have -and $have -lt $CosignMinimum) {
    Write-Step "note: cosign $have cannot read this release's signature bundle ($CosignMinimum or newer can), so only the checksum was verified"
    return
  }
  # Continue for the rest of this function only. Windows PowerShell 5.1 turns
  # each line a native program writes to a redirected stderr into an error
  # record, and under Stop the first one throws; cosign reports success on
  # stderr, so a signature that verified would fail the install. The exit
  # status is the answer. Each record is turned back into the line it came
  # from, so a refusal quotes cosign rather than PowerShell's wrapping of it.
  $ErrorActionPreference = 'Continue'
  $said = (& $cosign.Source verify-blob `
    --bundle $bundle `
    --certificate-identity-regexp $CosignIdentity `
    --certificate-oidc-issuer $CosignIssuer `
    (Join-Path $Directory 'checksums.txt') 2>&1 | ForEach-Object { "$_" }) -join "`n"
  if ($LASTEXITCODE -ne 0) {
    throw "cosign did not confirm that checksums.txt came from the release workflow, so nothing was installed. What cosign said:`n$said"
  }
  Write-Step 'signature verified with cosign'
}

# Get-CosignVersion is the version the cosign at this path reports, or $null
# when it reports none as X.Y.Z; a build from source says "devel", and is then
# taken to be new enough, as install.sh takes it.
function Get-CosignVersion {
  param([string]$Cosign)
  $ErrorActionPreference = 'Continue'
  $text = (& $Cosign version 2>$null | ForEach-Object { "$_" }) -join "`n"
  if ($text -match '(?m)^GitVersion:\s*v?(\d+\.\d+\.\d+)') { return [version]$Matches[1] }
  return $null
}

function Get-TargetDirectory {
  if ($BinDir) { return $BinDir }
  # Per user and under LOCALAPPDATA, because a machine-wide install needs an
  # elevated prompt and a script read off the network should not be the thing
  # that asks for one. Program Files is where an installer with a UAC prompt
  # belongs, not where this belongs.
  $base = $env:LOCALAPPDATA
  if (-not $base) { $base = [IO.Path]::GetTempPath() }
  return (Join-Path (Join-Path $base 'Programs') $Binary)
}

function Add-ToUserPath {
  param([string]$Directory)
  # Only on Windows: the user PATH is a registry value, and asking for it
  # anywhere else throws. On Windows it is also the right place, which is why
  # this does what a Unix script should not: there is a per-user PATH to edit
  # that belongs to the user and needs no shell startup file.
  $onWindows = $true
  if (Test-Path variable:IsWindows) { $onWindows = $IsWindows }
  elseif ($env:OS -ne 'Windows_NT') { $onWindows = $false }
  if (-not $onWindows) { return $false }

  $current = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (-not $current) { $current = '' }
  $already = $false
  foreach ($entry in $current -split ';') {
    if ($entry.Trim().TrimEnd('\') -ieq $Directory.TrimEnd('\')) { $already = $true; break }
  }
  if ($already) { return $false }

  $updated = $current.TrimEnd(';')
  if ($updated) { $updated = "$updated;$Directory" } else { $updated = $Directory }
  [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
  # This session too, so the command works without opening a new window.
  $env:Path = "$env:Path;$Directory"
  return $true
}

# On PATH is not the same as the one that runs, and on Windows the gap is
# wider than anywhere else: the machine PATH is read before the user one this
# script writes to, so a copy under Program Files, or an older `go install`
# build, keeps answering to the name. Nothing above would say so, because the
# line that reports the install names the file it just wrote, by its full path.
# install.sh makes the same check for the same reason.
function Show-ShadowWarning {
  param([Parameter(Mandatory = $true)][string] $Directory)
  $mine = Join-Path $Directory "$Binary.exe"
  # The object first and the property second, because nothing answering to the
  # name is the ordinary case and Set-StrictMode turns $null.Source into a
  # thrown error, which would fail the install this is only commenting on.
  $found = Get-Command $Binary -CommandType Application -ErrorAction SilentlyContinue |
    Select-Object -First 1
  if (-not $found) { return }
  $running = $found.Source
  if ($running -ieq $mine) { return }
  Write-Step ''
  Write-Step "warning: $Binary still runs $running, which comes earlier in your PATH."
  Write-Step "         Remove it, or put $Directory first, or run $mine by its full path."
  Write-Step ''
}

function Install-GhChronicle {
  $arch = Get-Architecture
  if (-not $Version) { $Version = Get-NewestVersion }
  $Version = $Version -replace '^v', ''
  $archive = "${Binary}_${Version}_windows_${arch}.zip"

  $work = Join-Path ([IO.Path]::GetTempPath()) ("ghchronicle-" + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $work | Out-Null
  try {
    Write-Step "downloading $Binary $Version for windows/$arch"
    try {
      Invoke-WebRequest -Uri "$DownloadBase/v$Version/$archive" `
        -OutFile (Join-Path $work $archive) -UseBasicParsing
    } catch {
      throw "no archive at $DownloadBase/v$Version/$archive. Check the version against the releases page."
    }
    try {
      Invoke-WebRequest -Uri "$DownloadBase/v$Version/checksums.txt" `
        -OutFile (Join-Path $work 'checksums.txt') -UseBasicParsing
    } catch {
      throw "the release publishes no checksums.txt, so what was downloaded cannot be checked"
    }

    Assert-Checksum -Directory $work -Archive $archive
    Write-Step 'checksum verified'
    Assert-Signature -Directory $work -Release $Version

    Expand-Archive -LiteralPath (Join-Path $work $archive) -DestinationPath (Join-Path $work 'x') -Force
    $exe = Join-Path (Join-Path $work 'x') "$Binary.exe"
    if (-not (Test-Path -LiteralPath $exe)) { throw "the archive does not contain $Binary.exe" }

    $target = Get-TargetDirectory
    if (-not (Test-Path -LiteralPath $target)) {
      New-Item -ItemType Directory -Path $target -Force | Out-Null
    }
    Copy-Item -LiteralPath $exe -Destination (Join-Path $target "$Binary.exe") -Force
    Write-Step "installed $(Join-Path $target "$Binary.exe")"

    if ($NoPathUpdate) {
      Write-Step "left your PATH alone, as asked. Run it by its full path, or add $target yourself."
      return
    }
    if (Add-ToUserPath -Directory $target) {
      Write-Step "added $target to your PATH. Open a new terminal for other programs to see it."
    }
    Show-ShadowWarning -Directory $target
  } finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
  }
}

# One line and a non-zero exit rather than PowerShell's default error block:
# this is read by somebody who piped a script into a shell, and the stack trace
# of a `throw` is noise in front of the sentence that matters. The same shape
# as install.sh, deliberately.
try {
  Install-GhChronicle
} catch {
  Write-Host "install: $($_.Exception.Message)" -ForegroundColor Red
  exit 1
}
