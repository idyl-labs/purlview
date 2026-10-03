# Installs Purlview through one public channel exactly as purlview.com tells
# people to, then checks the installed CLI: it is the expected release, it
# runs, and it answers without a sign-in. install-check.yml runs it on every
# Windows runner a channel supports; channel-check.sh is macOS and Linux.
#
#   channel-check.ps1 -Method <script|scoop|winget> -Version vX.Y.Z
#
# It changes the machine it runs on, so it runs on throwaway CI runners only.
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [ValidateSet('script', 'scoop', 'winget')] [string] $Method,
    [Parameter(Mandatory)] [string] $Version
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# Picks up what an installer added to the machine or user PATH, keeping
# what this session already has.
function Update-SessionPath {
    $env:Path = $env:Path + ';' + [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
}

# Assert-UserPathHolds checks the registry's user Path holds $Dir exactly
# $Times times, is still REG_EXPAND_SZ, and the machine Path doesn't hold it.
function Assert-UserPathHolds([string] $Dir, [int] $Times) {
    $key = Get-Item HKCU:\Environment
    $raw = $key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
    $count = @($raw -split ';' | Where-Object { [Environment]::ExpandEnvironmentVariables($_).TrimEnd('\') -ieq $Dir }).Count
    if ($count -ne $Times) { throw "the user Path holds $Dir $count times, want $Times" }
    if ($Times -gt 0 -and $key.GetValueKind('Path') -ne 'ExpandString') { throw "the user Path is $($key.GetValueKind('Path')), want ExpandString" }
    if (([Environment]::GetEnvironmentVariable('Path', 'Machine') -split ';') -contains $Dir) { throw 'the machine Path was changed' }
}

switch ($Method) {
    'script' {
        # In a fresh session, as a person pastes it (this script's own
        # variables must not meet the installer's): purlview must then run
        # in that same window with no further step.
        $exe = Join-Path $env:LOCALAPPDATA 'Programs\purlview\purlview.exe'
        $same = @(pwsh -NoProfile -Command 'irm https://purlview.com/install.ps1 | iex; (Get-Command purlview).Source; purlview --version')
        if ($LASTEXITCODE -ne 0) { throw "install.ps1 exited $LASTEXITCODE" }
        $same | Write-Output
        if ($same -notcontains $exe) { throw "purlview did not run in the window that installed it" }
        Write-Output 'ok   runs in the installing window with no manual step'
        # A new terminal reads PATH from the registry: the user Path holds the
        # folder once, still expandable; the machine Path is untouched.
        Assert-UserPathHolds (Split-Path $exe) 1
        $env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
        if ((Get-Command purlview -ErrorAction SilentlyContinue).Source -ne $exe) { throw 'a new terminal does not find purlview' }
        Write-Output 'ok   a new terminal finds purlview'
        pwsh -NoProfile -Command 'irm https://purlview.com/install.ps1 | iex' | Out-Null
        Assert-UserPathHolds (Split-Path $exe) 1
        Write-Output 'ok   a second install leaves one Path entry'
    }
    'scoop' {
        # Scoop itself first, as a person without it would. CI runners are
        # administrators, which Scoop's installer refuses without the flag.
        & ([scriptblock]::Create((Invoke-RestMethod https://get.scoop.sh))) -RunAsAdmin
        Update-SessionPath
        scoop bucket add idyl-labs https://github.com/idyl-labs/scoop-bucket
        scoop install purlview
    }
    'winget' {
        if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
            # Windows Server runners ship without WinGet; Microsoft's module
            # installs it the supported way.
            Install-PackageProvider -Name NuGet -Force | Out-Null
            Install-Module Microsoft.WinGet.Client -Force -Scope AllUsers
            Repair-WinGetPackageManager -AllUsers -Latest
        }
        winget install IdylLabs.Purlview --exact --accept-source-agreements --accept-package-agreements --disable-interactivity
        if ($LASTEXITCODE -ne 0) { throw "winget install IdylLabs.Purlview exited $LASTEXITCODE" }
    }
}
Update-SessionPath

$bin = Get-Command purlview -ErrorAction SilentlyContinue
if (-not $bin) { throw "purlview is not on PATH after the $Method install" }
Write-Output "ok   $Method installed $($bin.Source)"

$env:PURLVIEW_STATE_DIR = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
$env:PURLVIEW_NO_UPDATE_CHECK = '1'
$got = (& purlview --version | Select-Object -First 1)
$installed = $got -replace '^purlview ', ''
if ($installed -ne $Version -and $installed -ne $Version.TrimStart('v')) { throw "installed '$got', want $Version" }
Write-Output "ok   $got"
$out = (& purlview list 2>&1 | Out-String)
if ($LASTEXITCODE -eq 0) { throw "list succeeded without a sign-in: $out" }
if ($out -notmatch 'Not signed in') { throw "list without a sign-in said: $out" }
Write-Output 'ok   runs and asks for a sign-in'

if ($Method -eq 'script') {
    pwsh -NoProfile -Command '& ([scriptblock]::Create((irm https://purlview.com/install.ps1))) -Uninstall'
    if ($LASTEXITCODE -ne 0) { throw "install.ps1 -Uninstall exited $LASTEXITCODE" }
    Assert-UserPathHolds (Join-Path $env:LOCALAPPDATA 'Programs\purlview') 0
    Write-Output 'ok   -Uninstall removes the Path entry'
}
exit 0
