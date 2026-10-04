<#
.SYNOPSIS
Purlview installer for Windows.

.DESCRIPTION
Resolves the requested version (the latest stable release by default;
prereleases are never chosen implicitly), downloads the Windows archive and
checksums.txt for this architecture over HTTPS, verifies the SHA-256 and the
publisher's Authenticode signature, checks that the extracted executable runs,
stops the previous version's daemon (ending its active shares) while blocking
new daemon starts, and only then replaces Destination\purlview.exe. A
previous installation is left intact if any step fails, including when the
executable is in use, but shares that were ended stay ended.

The checksum comes from the same release as the archive, so it detects
corruption and truncation; it is not publisher authentication. That is the
Authenticode signature on the executable itself: it must be valid and
Purlview's publisher's before the downloaded executable runs for the first
time, and an executable that fails it is neither run nor installed.

If Destination is not on PATH, it is added to the current user's Path
(never the machine Path; no administrator rights) and to the PATH of this
session, so purlview works in this window right away. -Uninstall removes it
from the user Path again. -NoModifyPath or PURLVIEW_NO_MODIFY_PATH=1 leave
PATH alone.

.PARAMETER Version
Release to install, for example v0.2.0. Defaults to the latest stable release.

.PARAMETER Destination
Directory for purlview.exe. Defaults to $env:LOCALAPPDATA\Programs\purlview
(no elevation needed).

.PARAMETER Architecture
Override the detected architecture: amd64 or arm64.

.PARAMETER AddToPath
Add Destination to PATH even when PURLVIEW_NO_MODIFY_PATH is set. Adding it
is the default; the switch is kept for older instructions.

.PARAMETER NoModifyPath
Leave PATH alone, as PURLVIEW_NO_MODIFY_PATH=1 does.

.PARAMETER Uninstall
Remove purlview.exe from Destination, and Destination from the user Path.
Unless another purlview stays on PATH, also stop the daemon and remove its
runtime, log and cache directories. Account credentials are kept.

.EXAMPLE
irm https://purlview.com/install.ps1 | iex

.EXAMPLE
& ([scriptblock]::Create((irm https://purlview.com/install.ps1))) -Version v0.2.0 -Destination "C:\Tools\purlview"

.EXAMPLE
powershell -ExecutionPolicy Bypass -File install.ps1 -Uninstall
#>
[CmdletBinding()]
param(
    [string]$Version = $env:PURLVIEW_VERSION,
    [string]$Destination = $env:PURLVIEW_INSTALL_DIR,
    [ValidateSet('', 'amd64', 'arm64', 'x64', 'x86_64', 'aarch64')]
    [string]$Architecture = '',
    [switch]$AddToPath,
    [switch]$NoModifyPath,
    [switch]$Uninstall
)

# Piped to Invoke-Expression (`irm ... | iex`), this script runs in the
# caller's own session. Everything below runs in a child scope, so its
# preferences, strict mode, variables and functions leave nothing behind,
# and the module path it changes is restored however it ends. PowerShell
# parses the whole text before running any of it, so a download cut short
# runs nothing.
& {
param($bound)
$callerModulePath = $env:PSModulePath
try {

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

# Built-in commands such as Get-AuthenticodeSignature are resolved through
# PSModulePath. When this script is started from a PowerShell 7 session, the
# inherited PSModulePath lists PowerShell 7's modules first and Windows
# PowerShell 5.1 would try to load an incompatible module. Put this host's own
# module directory first while the installer runs.
$ownModules = Join-Path $PSHOME 'Modules'
$env:PSModulePath = (@($ownModules) + @(($env:PSModulePath -split ';') | Where-Object { $_ -and $_ -ne $ownModules })) -join ';'
Add-Type -AssemblyName System.IO.Compression.FileSystem

function Get-Sha256Hex([string]$Path) {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $stream = [System.IO.File]::OpenRead($Path)
    try { return ([System.BitConverter]::ToString($sha.ComputeHash($stream)) -replace '-', '').ToLowerInvariant() }
    finally { $stream.Dispose(); $sha.Dispose() }
}

$ReleaseBaseUrl = if ($env:PURLVIEW_RELEASE_BASE_URL) { $env:PURLVIEW_RELEASE_BASE_URL } else { 'https://github.com/idyl-labs/purlview-releases' }
$ReleaseBaseUrl = $ReleaseBaseUrl.TrimEnd('/')
# The name on the certificate that signs Purlview's Windows executables: the
# verified publisher Windows shows for them.
$Publisher = 'Idyl Labs, Inc.'
$DestinationExplicit = $bound.ContainsKey('Destination') -or [bool]$env:PURLVIEW_INSTALL_DIR
if (-not $Destination) { $Destination = Join-Path $env:LOCALAPPDATA 'Programs\purlview' }

function Write-InstallLog([string]$Message) { Write-Host "purlview-install: $Message" }
function Fail([string]$Message) { throw "purlview-install: error: $Message" }

# Only HTTPS is accepted, except plain HTTP to the loopback interface, which
# the installer tests use to serve local fixtures. Nothing signs a fixture, so
# its signature is checked only when PURLVIEW_VERIFY_PUBLISHER=1 asks.
if ($ReleaseBaseUrl -notmatch '^https://' -and $ReleaseBaseUrl -notmatch '^http://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]+)?(/|$)') {
    Fail "PURLVIEW_RELEASE_BASE_URL must be an https:// URL, got: $ReleaseBaseUrl"
}
$VerifyPublisher = $ReleaseBaseUrl -match '^https://' -or $env:PURLVIEW_VERIFY_PUBLISHER -eq '1'

$onWindows = $true
if (Get-Variable -Name IsWindows -ErrorAction SilentlyContinue) { $onWindows = [bool]$IsWindows }
if (-not $onWindows) { Fail 'this installer is for Windows; use install.sh on macOS and Linux' }

# --- host detection ------------------------------------------------------------

function Get-HostArchitecture {
    # OSArchitecture reports the operating system, so an x64 PowerShell running
    # under emulation on an Arm64 machine still selects the native arm64 build.
    $os = $null
    try { $os = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { $os = $null }
    if (-not $os) {
        $os = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    }
    switch -Regex ($os) {
        '^(Arm64|ARM64)$' { return 'arm64' }
        '^(X64|AMD64)$' { return 'amd64' }
        default { Fail "unsupported architecture: $os. Purlview supports amd64 and arm64 Windows; pass -Architecture to override." }
    }
}

if ($Architecture) {
    switch -Regex ($Architecture) {
        '^(amd64|x64|x86_64)$' { $Architecture = 'amd64' }
        '^(arm64|aarch64)$' { $Architecture = 'arm64' }
    }
} else {
    $Architecture = Get-HostArchitecture
}

# --- managed-install detection -------------------------------------------------

function Get-ManagedOwner([string]$Path) {
    if (-not $Path) { return '' }
    $full = [System.IO.Path]::GetFullPath($Path)
    $scoopRoot = if ($env:SCOOP) { $env:SCOOP } else { Join-Path $env:USERPROFILE 'scoop' }
    if ($full.StartsWith([System.IO.Path]::GetFullPath($scoopRoot), [System.StringComparison]::OrdinalIgnoreCase)) { return 'Scoop' }
    if ($env:SCOOP_GLOBAL -and $full.StartsWith([System.IO.Path]::GetFullPath($env:SCOOP_GLOBAL), [System.StringComparison]::OrdinalIgnoreCase)) { return 'Scoop' }
    $wingetPortable = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Packages'
    if ($full.StartsWith([System.IO.Path]::GetFullPath($wingetPortable), [System.StringComparison]::OrdinalIgnoreCase)) { return 'WinGet' }
    $wingetLinks = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Links'
    if ($full.StartsWith([System.IO.Path]::GetFullPath($wingetLinks), [System.StringComparison]::OrdinalIgnoreCase)) { return 'WinGet' }
    return ''
}

$target = Join-Path $Destination 'purlview.exe'

# --- daemon control ----------------------------------------------------------------

# Invoke-Daemon runs a daemon subcommand of the given executable and returns
# its combined output; $null when the executable does not know the command
# (an older build) or cannot run.
function Invoke-Daemon([string]$Exe, [string[]]$Arguments) {
    if (-not (Test-Path -LiteralPath $Exe)) { return $null }
    $out = New-TemporaryFile
    $err = New-TemporaryFile
    try {
        $p = Start-Process -FilePath $Exe -ArgumentList (@('daemon') + $Arguments) -NoNewWindow -Wait -PassThru -RedirectStandardOutput $out -RedirectStandardError $err
        $text = ([string](Get-Content -LiteralPath $out -Raw -ErrorAction SilentlyContinue)) + ([string](Get-Content -LiteralPath $err -Raw -ErrorAction SilentlyContinue))
        if ($p.ExitCode -eq 2 -and $text -match 'unknown command') { return $null }
        return [pscustomobject]@{ ExitCode = $p.ExitCode; Output = $text }
    } catch {
        return $null
    } finally {
        Remove-Item -LiteralPath $out, $err -Force -ErrorAction SilentlyContinue
    }
}

# Invoke-DaemonStop stops the daemon owned by the current user through the installed
# executable, ending its active shares, and blocks new daemon starts until
# Invoke-DaemonResume runs or the marker expires. Stopping releases the executable's
# file handle, so the file can be replaced rather than only renamed.
function Invoke-DaemonStop([string]$Exe) {
    $r = Invoke-Daemon $Exe @('stop', '--maintenance', '--timeout', '20s')
    if ($null -eq $r) { return }
    if ($r.ExitCode -ne 0) { Write-InstallLog "warning: could not stop the Purlview daemon: $($r.Output.Trim())"; return }
    if ($r.Output -notmatch 'was not running') { Write-InstallLog 'stopped the running Purlview daemon (active shares ended)' }
}

function Invoke-DaemonResume([string]$Exe) {
    Invoke-Daemon $Exe @('resume') | Out-Null
}

# Get-RuntimeStatePath returns the daemon's runtime, log and cache
# directories for this user, as the CLI lays them out; these are installation
# artifacts, not user data. The directory that holds the account credential
# (credentials.json) is not among them, nor is anything else next to them.
function Get-RuntimeStatePath {
    $base = if ($env:PURLVIEW_STATE_DIR) { $env:PURLVIEW_STATE_DIR } else { Join-Path $env:LOCALAPPDATA 'purlview' }
    return @((Join-Path $base 'runtime'), (Join-Path $base 'logs'), (Join-Path $base 'cache'))
}

# --- PATH ------------------------------------------------------------------------

# Like rustup and uv, the installer puts Destination on the user Path
# (HKCU\Environment; never the machine Path, no administrator rights) and on
# the PATH of this session, which `irm ... | iex` shares with the caller.
# The user Path is read and written raw: it is usually REG_EXPAND_SZ with
# entries such as %USERPROFILE%\bin, which [Environment]::SetEnvironmentVariable
# would expand and store as REG_SZ.
$modifyPath = (-not $NoModifyPath) -and ($AddToPath -or -not ($env:PURLVIEW_NO_MODIFY_PATH -and $env:PURLVIEW_NO_MODIFY_PATH -ne '0'))
$pathDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Destination)
if ($pathDir.Length -gt 3) { $pathDir = $pathDir.TrimEnd('\', '/') }

# Test-PathEntry reports whether the ;-separated List names Directory,
# ignoring case and a trailing backslash, with %VARIABLES% expanded.
function Test-PathEntry([string]$List, [string]$Directory) {
    $want = $Directory.TrimEnd('\')
    foreach ($entry in ($List -split ';')) {
        if ($entry -and ([Environment]::ExpandEnvironmentVariables($entry).TrimEnd('\') -eq $want)) { return $true }
    }
    return $false
}

function Get-UserPath {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
    if ($null -eq $key) { return '' }
    try { return [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames) }
    finally { $key.Dispose() }
}

# Test-SavedPath reports whether the user or the machine Path names Directory.
function Test-SavedPath([string]$Directory) {
    return (Test-PathEntry (Get-UserPath) $Directory) -or (Test-PathEntry ([Environment]::GetEnvironmentVariable('Path', 'Machine')) $Directory)
}

# Write-UserPath stores the user Path, keeping its registry type, then tells
# running programs such as Explorer that the environment changed, so
# terminals started from now on have it. Removing a variable that does not
# exist sends that message and changes nothing.
function Write-UserPath([string]$Value) {
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
        if ($null -ne $key.GetValue('Path')) { $kind = $key.GetValueKind('Path') }
        $key.SetValue('Path', $Value, $kind)
    } finally {
        $key.Dispose()
    }
    [Environment]::SetEnvironmentVariable('PURLVIEW_INSTALLER_NOTICE', [NullString]::Value, 'User')
}

# Add-UserPathEntry appends Directory to the user Path unless the user or
# the machine Path names it already, and says whether it did.
function Add-UserPathEntry([string]$Directory) {
    if (Test-SavedPath $Directory) { return $false }
    $user = Get-UserPath
    $value = if (-not $user) { $Directory } elseif ($user.EndsWith(';')) { $user + $Directory } else { $user + ';' + $Directory }
    Write-UserPath $value
    return $true
}

# Clear-UserPathEntry removes the entry Add-UserPathEntry writes for
# Directory from the user Path, and says whether there was one.
function Clear-UserPathEntry([string]$Directory) {
    $entries = @((Get-UserPath) -split ';')
    $kept = @($entries | Where-Object { $_.TrimEnd('\') -ne $Directory.TrimEnd('\') })
    if ($kept.Count -eq $entries.Count) { return $false }
    Write-UserPath ($kept -join ';')
    return $true
}

# --- uninstall -------------------------------------------------------------------

if ($Uninstall) {
    if (-not (Test-Path -LiteralPath $target)) {
        # The executable is gone already; the Path entry may not be.
        if (Clear-UserPathEntry $pathDir) {
            Write-InstallLog "removed $pathDir from your user Path"
            return
        }
        Fail "nothing to remove at $target"
    }
    $owner = Get-ManagedOwner $target
    if ($owner) { Fail "$target is managed by $owner; remove it with that package manager" }
    # The daemon and its runtime state are this user's, not this copy's: while
    # another purlview stays on PATH they are that copy's too, and are left.
    $full = [System.IO.Path]::GetFullPath($target)
    $other = @(Get-Command purlview -All -CommandType Application -ErrorAction SilentlyContinue |
        Where-Object { [System.IO.Path]::GetFullPath($_.Source) -ne $full }) | Select-Object -First 1
    if (-not $other) { Invoke-DaemonStop $target }
    Remove-Item -LiteralPath $target -Force
    Remove-Item -LiteralPath "$target.old" -Force -ErrorAction SilentlyContinue
    Write-InstallLog "removed $target"
    if ($other) {
        Write-InstallLog "another purlview remains at $($other.Source): the daemon and its runtime state were left alone"
    } else {
        foreach ($dir in Get-RuntimeStatePath) {
            if (Test-Path -LiteralPath $dir) {
                Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
                Write-InstallLog "removed runtime state $dir"
            }
        }
    }
    if (Clear-UserPathEntry $pathDir) {
        Write-InstallLog "removed $pathDir from your user Path"
    } elseif (Test-PathEntry $env:Path $pathDir) {
        Write-InstallLog "if $Destination was added to PATH only for Purlview, remove it from your user Path"
    }
    Write-InstallLog "account credentials (if any) are not removed by uninstall; sign out with 'purlview logout' before uninstalling to revoke them"
    return
}

# --- networking ------------------------------------------------------------------

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { Write-Verbose 'could not force TLS 1.2; relying on defaults' }

function Invoke-Download([string]$Uri, [string]$OutFile) {
    $attempt = 0
    while ($true) {
        $attempt++
        try {
            Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing -TimeoutSec 300 -ErrorAction Stop
            return
        } catch {
            $status = $null
            try { $status = [int]$_.Exception.Response.StatusCode } catch { $status = $null }
            if ($status -eq 404) { throw "not found: $Uri" }
            if ($attempt -ge 3) { throw "download failed after $attempt attempts: $Uri ($($_.Exception.Message))" }
            Start-Sleep -Seconds (2 * $attempt)
        }
    }
}

function Resolve-LatestVersion {
    $uri = "$ReleaseBaseUrl/releases/latest"
    $request = [System.Net.HttpWebRequest]::Create($uri)
    $request.Method = 'HEAD'
    $request.AllowAutoRedirect = $false
    $request.Timeout = 60000
    $request.Proxy = [System.Net.WebRequest]::DefaultWebProxy
    if ($request.Proxy) { $request.Proxy.Credentials = [System.Net.CredentialCache]::DefaultCredentials }
    try {
        $response = $request.GetResponse()
    } catch {
        $status = $null
        try { $status = [int]$_.Exception.InnerException.Response.StatusCode } catch { $status = $null }
        if (-not $status) { try { $status = [int]$_.Exception.Response.StatusCode } catch { $status = $null } }
        if ($status -eq 404) { Fail "no stable release is published at $ReleaseBaseUrl yet; pass -Version to install a specific release" }
        Fail "could not reach $uri ($($_.Exception.Message))"
    }
    try {
        $location = $response.Headers['Location']
    } finally {
        $response.Close()
    }
    # A published stable release redirects to .../releases/tag/<tag>; with none,
    # GitHub redirects to the release list instead.
    if (-not $location -or $location -notmatch '/releases/tag/[^/]+/?$') { Fail "no stable release is published at $ReleaseBaseUrl yet; pass -Version to install a specific release" }
    return $location.TrimEnd('/').Split('/')[-1]
}

# --- version resolution --------------------------------------------------------

if (-not $Version) {
    $Version = Resolve-LatestVersion
    Write-InstallLog "latest stable release: $Version"
}
if ($Version -notmatch '^v') { $Version = "v$Version" }
if ($Version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') { Fail "invalid version $Version; expected vX.Y.Z or vX.Y.Z-prerelease" }
$bareVersion = $Version.Substring(1)
if ($bareVersion -match '-') { Write-InstallLog "installing prerelease $Version because it was requested explicitly" }

# --- existing installation --------------------------------------------------

$existing = Get-Command purlview -ErrorAction SilentlyContinue
if ($existing -and $existing.Source -and ([System.IO.Path]::GetFullPath($existing.Source) -ne [System.IO.Path]::GetFullPath($target))) {
    $owner = Get-ManagedOwner $existing.Source
    if ($owner) {
        if ($DestinationExplicit) {
            Write-InstallLog "warning: $($existing.Source) is managed by $owner; installing a second copy to $Destination as requested"
        } else {
            Fail "$($existing.Source) is already installed by $owner. Update it with $owner instead, or pass -Destination to install a separate copy."
        }
    } else {
        Write-InstallLog "note: another purlview is on PATH at $($existing.Source); the copy in $Destination wins only if it comes first in PATH"
    }
}
$destOwner = Get-ManagedOwner $target
if ($destOwner) { Fail "$target is managed by $destOwner; refusing to overwrite it" }

# --- download and verify ----------------------------------------------------

$archive = "purlview_${bareVersion}_windows_${Architecture}.zip"
$downloadBase = "$ReleaseBaseUrl/releases/download/$Version"
$work = Join-Path ([System.IO.Path]::GetTempPath()) ("purlview-install-" + [System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $work | Out-Null

try {
    Write-InstallLog "downloading $archive from $downloadBase"
    try { Invoke-Download "$downloadBase/checksums.txt" (Join-Path $work 'checksums.txt') }
    catch { Fail "could not download checksums.txt for $Version from $ReleaseBaseUrl (is the version published, and is the repository public?): $($_.Exception.Message)" }
    try { Invoke-Download "$downloadBase/$archive" (Join-Path $work $archive) }
    catch { Fail "could not download ${archive}; $Version may not provide windows/${Architecture}: $($_.Exception.Message)" }

    $expected = $null
    foreach ($line in Get-Content -LiteralPath (Join-Path $work 'checksums.txt')) {
        $parts = $line -split '\s+', 2
        if ($parts.Count -eq 2 -and ($parts[1] -eq $archive -or $parts[1] -eq "*$archive")) { $expected = $parts[0].ToLowerInvariant(); break }
    }
    if (-not $expected) { Fail "$archive is not listed in checksums.txt for $Version" }
    $actual = Get-Sha256Hex (Join-Path $work $archive)
    if ($actual -ne $expected) { Fail "checksum mismatch for $archive (expected $expected, got $actual); the download is corrupt or incomplete" }
    Write-InstallLog 'checksum verified'

    $extract = Join-Path $work 'extract'
    [System.IO.Compression.ZipFile]::ExtractToDirectory((Join-Path $work $archive), $extract)
    $exe = Join-Path $extract 'purlview.exe'
    if (-not (Test-Path -LiteralPath $exe)) { Fail "$archive does not contain purlview.exe" }

    # The executable has not run yet.
    if ($VerifyPublisher) {
        $signature = Get-AuthenticodeSignature -LiteralPath $exe
        $signer = 'none'
        if ($signature.SignerCertificate) {
            $signer = $signature.SignerCertificate.GetNameInfo([System.Security.Cryptography.X509Certificates.X509NameType]::SimpleName, $false)
        }
        if ($signature.Status -ne 'Valid' -or $signer -cne $Publisher) {
            Fail "purlview.exe in $archive is not signed by Purlview's publisher, $Publisher (signature status '$($signature.Status)', signer '$signer'); it was not run and nothing was installed"
        }
        Write-InstallLog "publisher signature verified: $signer"
    }

    $stdout = Join-Path $work 'version.out'
    $stderr = Join-Path $work 'version.err'
    $proc = Start-Process -FilePath $exe -ArgumentList '--version' -NoNewWindow -Wait -PassThru -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    $reported = if (Test-Path -LiteralPath $stdout) { [string](Get-Content -LiteralPath $stdout -TotalCount 1) } else { '' }
    if ($proc.ExitCode -ne 0 -or $reported -ne "purlview $bareVersion") {
        Fail "the downloaded executable did not run correctly on this machine (exit $($proc.ExitCode), reported '$reported'); nothing was installed. Check -Architecture or the installation guide."
    }

    # --- stop the previous version's daemon ----------------------------------

    # The new release is downloaded and verified; from here on daemon starts
    # are blocked and the running daemon (with its shares) is stopped, which
    # also releases the executable's file handle. If replacement fails, the
    # previous executable stays usable; ended shares stay ended.
    $maintenance = $false
    if (Test-Path -LiteralPath $target) {
        Write-InstallLog 'note: upgrading Purlview ends any active shares'
        Invoke-DaemonStop $target
        $maintenance = $true
    }

    # --- install atomically --------------------------------------------------

    try {
        if (-not (Test-Path -LiteralPath $Destination)) {
            try { New-Item -ItemType Directory -Path $Destination -Force | Out-Null } catch { Fail "cannot create ${Destination}: $($_.Exception.Message)" }
        }
        $staged = "$target.new"
        try { Copy-Item -LiteralPath $exe -Destination $staged -Force } catch { Fail "cannot write to ${Destination}: $($_.Exception.Message)" }
        Remove-Item -LiteralPath "$target.old" -Force -ErrorAction SilentlyContinue
        try {
            if (Test-Path -LiteralPath $target) {
                # With the daemon stopped the old file is normally free. A
                # short-lived CLI process may still hold it; a running
                # executable cannot be overwritten, but it can be renamed.
                Move-Item -LiteralPath $target -Destination "$target.old" -Force
            }
            Move-Item -LiteralPath $staged -Destination $target -Force
        } catch {
            Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
            if ((Test-Path -LiteralPath "$target.old") -and -not (Test-Path -LiteralPath $target)) {
                Move-Item -LiteralPath "$target.old" -Destination $target -Force -ErrorAction SilentlyContinue
            }
            Fail "could not replace $target (is purlview running? close it and run the installer again). The previous installation is unchanged: $($_.Exception.Message)"
        }
        Remove-Item -LiteralPath "$target.old" -Force -ErrorAction SilentlyContinue
    } finally {
        if ($maintenance) {
            if (Test-Path -LiteralPath $target) { Invoke-DaemonResume $target } else { Invoke-DaemonResume $exe }
        }
    }
    Write-InstallLog "installed purlview $bareVersion to $target"

    # --- PATH --------------------------------------------------------------

    # `powershell -File install.ps1` runs in a process of its own, whose PATH
    # ends with it; there only a new terminal has the new user Path.
    $fromFile = [bool](Get-Variable -Name PSCommandPath -ValueOnly -ErrorAction SilentlyContinue)
    $ownProcess = $fromFile -and (@([Environment]::GetCommandLineArgs() | Where-Object { $_ -match '^(-{1,2}|/)f(i|il|ile)?$' }).Count -gt 0)
    $inSession = Test-PathEntry $env:Path $pathDir
    if ($modifyPath) {
        if (Add-UserPathEntry $pathDir) { Write-InstallLog "added $pathDir to your user Path" }
        if (-not $inSession -and -not $ownProcess) {
            $env:Path = "$pathDir;$env:Path"
            $inSession = $true
        }
    } elseif (-not $inSession -and -not (Test-SavedPath $pathDir)) {
        Write-InstallLog "$pathDir is not on your PATH. Add it to your user Path in Settings > Environment Variables."
    }
    Write-InstallLog 'update: run this installer again; pin: -Version vX.Y.Z; remove: -Uninstall'
    if ($inSession) {
        Write-InstallLog "next: run 'purlview login'"
    } else {
        Write-InstallLog "next: open a new terminal, then run 'purlview login'"
    }
} finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
} finally {
    $env:PSModulePath = $callerModulePath
}
} $PSBoundParameters
