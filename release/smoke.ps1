<#
.SYNOPSIS
Smoke-test Purlview Windows release artifacts on this machine.

.DESCRIPTION
Verifies checksums.txt for the Windows archive of the given architecture,
expands it, runs help/version/completion/placeholder checks, proves the
executable starts and stops its own daemon in a private state directory, and
verifies the Authenticode signature unless -AllowUnsigned is given.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$Dist,
    [Parameter(Mandatory)] [string]$Version,
    [string]$Architecture = '',
    [switch]$AllowUnsigned
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

if (-not $Architecture) {
    $os = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    $Architecture = if ($os -match '^Arm64$') { 'arm64' } else { 'amd64' }
}
$script:failed = $false
function Pass([string]$m) { Write-Host "ok    $m" }
function Fail([string]$m) { Write-Host "FAIL  $m" -ForegroundColor Red; $script:failed = $true }
function Note([string]$m) { Write-Host "note  $m" }

# Invoke-Native runs the executable and waits for that process only.
# Start-Process -Wait would wait for the whole process tree through a job
# object, which includes a daemon the command started and would stall until
# the daemon's idle exit.
function Invoke-Native([string]$Exe, [string[]]$Arguments, [hashtable]$Environment = @{}) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $Exe
    $psi.Arguments = (@($Arguments) | ForEach-Object { if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ } }) -join ' '
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    foreach ($k in $Environment.Keys) { $psi.EnvironmentVariables[$k] = [string]$Environment[$k] }
    $p = [System.Diagnostics.Process]::Start($psi)
    $p.StandardInput.Close()
    $stdout = $p.StandardOutput.ReadToEndAsync()
    $stderr = $p.StandardError.ReadToEndAsync()
    $p.WaitForExit()
    [pscustomobject]@{
        ExitCode = $p.ExitCode
        StdOut   = [string]$stdout.Result
        StdErr   = [string]$stderr.Result
    }
}

$Dist = (Resolve-Path -LiteralPath $Dist).Path
$archive = "purlview_${Version}_windows_${Architecture}.zip"
$archivePath = Join-Path $Dist $archive
if (-not (Test-Path -LiteralPath $archivePath)) { throw "archive not found: $archivePath" }

Write-Host "== checksums (windows/$Architecture) =="
$expected = $null
foreach ($line in Get-Content -LiteralPath (Join-Path $Dist 'checksums.txt')) {
    $parts = $line -split '\s+', 2
    if ($parts.Count -eq 2 -and $parts[1] -eq $archive) { $expected = $parts[0].ToLowerInvariant() }
}
$actual = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($expected -and $expected -eq $actual) { Pass "checksums.txt matches $archive" } else { Fail "checksum mismatch for $archive (expected $expected, got $actual)" }

Write-Host '== archive =='
$work = Join-Path ([System.IO.Path]::GetTempPath()) ("purlview-smoke-" + [System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $work | Out-Null
try {
    Expand-Archive -LiteralPath $archivePath -DestinationPath $work -Force
    foreach ($f in 'purlview.exe', 'completions\purlview.ps1', 'completions\purlview.bash', 'man\purlview.1') {
        if (Test-Path -LiteralPath (Join-Path $work $f)) { Pass "archive contains $f" } else { Fail "archive missing $f" }
    }
    $exe = Join-Path $work 'purlview.exe'

    $r = Invoke-Native $exe @('--version')
    $first = if ($r.StdOut) { ($r.StdOut -split "`r?`n")[0] } else { '' }
    if ($r.ExitCode -eq 0 -and $first -eq "purlview $Version" -and -not $r.StdErr) { Pass "--version reports $Version" } else { Fail "--version exit=$($r.ExitCode) first='$first' stderr='$($r.StdErr)'" }
    if ($r.StdOut -match 'built by: goreleaser') { Pass 'build identity present' } else { Fail "build identity missing: $($r.StdOut)" }

    $r = Invoke-Native $exe @('--help')
    if ($r.ExitCode -eq 0 -and $r.StdOut -match '(?m)^Usage:' -and -not $r.StdErr) { Pass '--help' } else { Fail "--help exit=$($r.ExitCode)" }

    $r = Invoke-Native $exe @()
    if ($r.ExitCode -eq 0 -and $r.StdOut -match '(?m)^Usage:') { Pass 'no arguments shows help' } else { Fail "bare invocation exit=$($r.ExitCode)" }

    foreach ($shell in 'bash', 'zsh', 'fish', 'powershell') {
        $r = Invoke-Native $exe @('completion', $shell)
        if ($r.ExitCode -eq 0 -and $r.StdOut.Length -gt 200 -and -not $r.StdErr) { Pass "completion $shell" } else { Fail "completion $shell exit=$($r.ExitCode)" }
    }
    $r = Invoke-Native $exe @('completion', 'powershell')
    $tokens = $null; $errors = $null
    [System.Management.Automation.Language.Parser]::ParseInput($r.StdOut, [ref]$tokens, [ref]$errors) | Out-Null
    if ($errors.Count -eq 0) { Pass 'powershell completion parses' } else { Fail "powershell completion has $($errors.Count) parse errors" }

    # The account commands read this user's remembered credential, so they
    # run against a private, empty state directory: signed out, they give
    # login instructions; login with closed stdin must issue no credential. share
    # starts a daemon and runs below with private state.
    $astate = Join-Path $work 'account-state'
    $aenv = @{ PURLVIEW_STATE_DIR = $astate }
    foreach ($placeholder in 'list', 'whoami') {
        $r = Invoke-Native $exe @($placeholder) $aenv
        if ($r.ExitCode -eq 1 -and $r.StdErr -match 'Not signed in' -and $r.StdErr -match 'run purlview login first' -and -not $r.StdOut) { Pass "$placeholder signed out gives login instructions (exit 1, stderr only)" } else { Fail "$placeholder exit=$($r.ExitCode) stdout='$($r.StdOut)' stderr='$($r.StdErr)'" }
    }
    $r = Invoke-Native $exe @('login') $aenv
    if ($r.ExitCode -eq 1 -and $r.StdErr -match '(?m)^  Email: \r?$' -and $r.StdErr -match 'Sign-in was not finished' -and -not $r.StdOut) { Pass 'email login stops on closed input (exit 1, stderr only)' } else { Fail "login exit=$($r.ExitCode) stdout='$($r.StdOut)' stderr='$($r.StdErr)'" }
    if (Test-Path -LiteralPath $astate) { Fail 'account commands created state' }

    $r = Invoke-Native $exe @('bogus')
    if ($r.ExitCode -eq 2 -and $r.StdErr -match "isn't a purlview command") { Pass 'unknown command exits 2' } else { Fail "unknown command exit=$($r.ExitCode)" }

    Write-Host '== daemon lifecycle (private state, no network) =='
    $state = Join-Path $work 'state'
    $denv = @{ PURLVIEW_STATE_DIR = $state; PURLVIEW_UPDATE_API_URL = 'http://127.0.0.1:1'; PURLVIEW_DAEMON_IDLE_EXIT = '30s'; PURLVIEW_DAEMON_START_TIMEOUT = '20s' }
    $r = Invoke-Native $exe @('daemon', 'status') $denv
    if ($r.ExitCode -eq 1 -and $r.StdErr -match 'not running') { Pass 'daemon status reports not running' } else { Fail "daemon status before start exit=$($r.ExitCode) $($r.StdErr)" }
    if (Test-Path -LiteralPath $state) { Fail 'daemon status created state' }
    # Signed out and without a terminal, share bootstraps the daemon and then
    # stops with login instructions; nothing is claimed on stdout.
    $r = Invoke-Native $exe @('share', 'localhost:3000') $denv
    if ($r.ExitCode -eq 1 -and $r.StdErr -match 'Not signed in' -and $r.StdErr -match 'run purlview login first' -and -not $r.StdOut) { Pass 'share starts the daemon and stops honestly (signed out)' } else { Fail "share with daemon exit=$($r.ExitCode) stdout='$($r.StdOut)' stderr='$($r.StdErr)'" }
    $r = Invoke-Native $exe @('daemon', 'status') $denv
    if ($r.ExitCode -eq 0 -and $r.StdOut -match "version $([regex]::Escape($Version))" -and $r.StdOut -match 'console:\s+none' -and $r.StdOut -match 'named pipe') { Pass "daemon running after share ($Version, no console, named pipe)" } else { Fail "daemon status after share exit=$($r.ExitCode) $($r.StdOut) $($r.StdErr)" }
    $r = Invoke-Native $exe @('daemon', 'start') $denv
    if ($r.ExitCode -eq 0 -and $r.StdOut -match 'daemon reused') { Pass 'second start reuses the daemon' } else { Fail "daemon start exit=$($r.ExitCode) $($r.StdOut) $($r.StdErr)" }
    # The stopped daemon must release its handle on the executable: the
    # file can then be renamed and deleted, which an upgrade relies on.
    $r = Invoke-Native $exe @('daemon', 'stop') $denv
    if ($r.ExitCode -eq 0 -and $r.StdOut -match 'stopped') { Pass 'daemon stop' } else { Fail "daemon stop exit=$($r.ExitCode) $($r.StdOut) $($r.StdErr)" }
    $r = Invoke-Native $exe @('daemon', 'status') $denv
    if ($r.ExitCode -eq 1) { Pass 'daemon gone after stop' } else { Fail "daemon still reported after stop: $($r.StdOut)" }
    if (Test-Path -LiteralPath (Join-Path $state 'runtime\endpoint.json')) { Fail 'endpoint record left after stop' }
    $copy = Join-Path $work 'purlview-copy.exe'
    Copy-Item -LiteralPath $exe -Destination $copy
    $r = Invoke-Native $copy @('daemon', 'start') $denv
    if ($r.ExitCode -eq 0 -and $r.StdOut -match 'daemon started') { Pass 'daemon starts from a copied executable' } else { Fail "copy start exit=$($r.ExitCode) $($r.StdOut) $($r.StdErr)" }
    $moved = Join-Path $work 'purlview-moved.exe'
    $renamed = $false
    try { Move-Item -LiteralPath $copy -Destination $moved -Force; $renamed = $true } catch { $renamed = $false }
    if ($renamed) { Pass 'running daemon executable can be renamed' } else { Fail 'running daemon executable could not be renamed' }
    $deletedWhileRunning = $true
    try { Remove-Item -LiteralPath $moved -Force -ErrorAction Stop } catch { $deletedWhileRunning = $false }
    if (-not $deletedWhileRunning) { Pass 'running daemon executable cannot be deleted (handle held, as expected)' } else { Note 'running daemon executable was deletable on this system' }
    $r = Invoke-Native $exe @('daemon', 'stop') $denv
    if ($r.ExitCode -eq 0 -and $r.StdOut -match 'retired|stopped') { Pass 'installed executable retires and stops the daemon started from the copy' } else { Fail "stop after copy start exit=$($r.ExitCode) $($r.StdOut) $($r.StdErr)" }
    if (Test-Path -LiteralPath $moved) {
        $deletedAfterStop = $true
        try { Remove-Item -LiteralPath $moved -Force -ErrorAction Stop } catch { $deletedAfterStop = $false }
        if ($deletedAfterStop) { Pass 'stopped daemon released its executable handle (file deleted)' } else { Fail 'executable still locked after the daemon stopped' }
    }
    $log = Join-Path $state 'logs\daemon.log'
    if ((Test-Path -LiteralPath $log) -and ((Get-Content -LiteralPath $log -Raw) -match 'exited \(requested\)')) { Pass 'daemon log records the requested exit' } else { Fail 'daemon log incomplete' }

    Write-Host '== Authenticode =='
    $sig = Get-AuthenticodeSignature -LiteralPath $exe
    if ($sig.Status -eq 'Valid') {
        Pass "signature valid: $($sig.SignerCertificate.Subject); timestamp: $($sig.TimeStamperCertificate.Subject)"
    } elseif ($AllowUnsigned) {
        Note "signature status $($sig.Status) (allowed for this rehearsal)"
    } else {
        Fail "signature status $($sig.Status)"
    }
} finally {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}

if ($script:failed) { Write-Host "smoke: FAILURES for windows/$Architecture $Version"; exit 1 }
Write-Host "smoke: all checks passed for windows/$Architecture $Version"
