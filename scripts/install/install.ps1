# Olsyn Drive installer for Windows (per user, no administrator rights).
#
#   irm https://drive.olsyn.com/install.ps1 | iex
#   powershell -ExecutionPolicy Bypass -File install.ps1 [-Server <url>] [-Channel stable|beta] [-NoLaunch] [-Force]
#
# Works in Windows PowerShell 5.1 and PowerShell 7, under the default
# execution policy when piped (irm | iex) or started with -ExecutionPolicy
# Bypass, through the system proxy, over TLS 1.2 or later.
#
# What it does:
#   1. reads the release manifest: GET <server>/api/client/v1/releases/<channel>.json
#   2. picks the installer for this PC (windows-x64 or windows-arm64)
#   3. downloads it to a temporary folder and checks its size and SHA-256 against the manifest
#   4. checks its Authenticode signature when it has one (and says so plainly when it has none)
#   5. runs "olsyn-drive.exe install --user --quiet --manifest <saved manifest>"
#   6. deletes the temporary folder and opens Olsyn Drive
#
# The script does NOT check the manifest's Ed25519 signature: Windows
# PowerShell 5.1 has no Ed25519. It passes the manifest, exactly as the
# drive served it, to the installer, and the installer checks the signature
# with the keys compiled into it and refuses to install when it does not
# check out, or when the downloaded file is not the one the signed manifest
# names. Running it again updates an older installed copy. Nothing is sent
# anywhere except the requests above. It never asks for administrator rights.
#
# Exit codes: 0 installed (or already up to date), 1 failed, 2 not supported on this PC.
# When piped into iex the exit code is left in $LASTEXITCODE and the window stays open.

# The drive server fills this in when it serves the script; the default is used when it did not.
$DriveBase = '__DRIVE_BASE__'

function Install-OlsynDrive {
    [CmdletBinding()]
    param(
        # The drive server to install from (default: the one this script came from).
        [string]$Server = '',
        # Release channel: stable or beta.
        [string]$Channel = '',
        # Install, but do not open Olsyn Drive afterwards.
        [switch]$NoLaunch,
        # Install even when the same version is already installed.
        [switch]$Force,
        # Development builds only: install a build that has no release keys to check the manifest with.
        [switch]$AllowUnverified
    )

    $ErrorActionPreference = 'Stop'
    $oldProgress = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue' # the progress bar makes downloads very slow in Windows PowerShell 5.1

    function Say([string]$text) { Write-Host $text }
    function Fail([string]$text) { Write-Host ('Olsyn Drive was not installed: ' + $text) -ForegroundColor Red; return 1 }

    # --- Where from ------------------------------------------------------------
    if (-not $Server) { $Server = $env:OLSYN_DRIVE_SERVER }
    if (-not $Server) { $Server = $DriveBase }
    if ($Server -notmatch '^https?://') { $Server = 'https://drive.olsyn.com' }
    $Server = $Server.TrimEnd('/')
    if (-not $Channel) { $Channel = $env:OLSYN_DRIVE_CHANNEL }
    if (-not $Channel) { $Channel = 'stable' }
    if ($Channel -notmatch '^[a-z]+$') { return (Fail "unknown channel '$Channel'.") }

    # --- This PC ---------------------------------------------------------------
    $onWindows = ($PSVersionTable.PSEdition -eq 'Desktop') -or ($IsWindows -eq $true)
    if (-not $onWindows) {
        Write-Host 'Olsyn Drive is available for Windows only for now. macOS and Linux are not available yet.'
        return 2
    }
    $arch = ''
    try {
        $arch = (Get-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment' -Name PROCESSOR_ARCHITECTURE).PROCESSOR_ARCHITECTURE
    } catch { }
    if (-not $arch) { $arch = $env:PROCESSOR_ARCHITEW6432 }
    if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
    switch ($arch.ToUpperInvariant()) {
        'AMD64' { $platform = 'windows-x64' }
        'ARM64' { $platform = 'windows-arm64' }
        default {
            Write-Host "Olsyn Drive needs 64-bit Windows (x64 or Arm). This PC reports '$arch'."
            return 2
        }
    }
    $elevated = $false
    try {
        $id = [Security.Principal.WindowsIdentity]::GetCurrent()
        $elevated = (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    } catch { }
    if ($elevated) {
        return (Fail 'this PowerShell window runs as administrator. Olsyn Drive installs for you only and needs no administrator rights: run the same command in a normal PowerShell window.')
    }

    # --- Network: TLS 1.2 or later, the system proxy -----------------------------
    try {
        $tls = [Net.SecurityProtocolType]::Tls12
        if ([enum]::GetNames([Net.SecurityProtocolType]) -contains 'Tls13') { $tls = $tls -bor [Net.SecurityProtocolType]::Tls13 }
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor $tls
    } catch { }
    try {
        $proxy = [Net.WebRequest]::GetSystemWebProxy()
        $proxy.Credentials = [Net.CredentialCache]::DefaultCredentials
        [Net.WebRequest]::DefaultWebProxy = $proxy
    } catch { }

    $temp = Join-Path ([IO.Path]::GetTempPath()) ('olsyn-drive-install-' + [Guid]::NewGuid().ToString('N'))
    try {
        New-Item -ItemType Directory -Path $temp -Force | Out-Null

        # --- 1. The release manifest -------------------------------------------
        $manifestUrl = "$Server/api/client/v1/releases/$Channel.json"
        Say "Looking for the latest Olsyn Drive ($Channel) on $Server"
        try {
            $resp = Invoke-WebRequest -Uri $manifestUrl -UseBasicParsing -Headers @{ Accept = 'application/json' }
        } catch {
            return (Fail ("could not read $manifestUrl. Check this PC is online. (" + $_.Exception.Message + ')'))
        }
        $raw = $resp.Content
        if ($raw -is [byte[]]) { $raw = [Text.Encoding]::UTF8.GetString($raw) }
        $manifestFile = Join-Path $temp "$Channel.json"
        [IO.File]::WriteAllText($manifestFile, $raw, (New-Object Text.UTF8Encoding $false))
        try {
            $envelope = $raw | ConvertFrom-Json
            $manifestText = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($envelope.manifest))
            $manifest = $manifestText | ConvertFrom-Json
        } catch {
            return (Fail 'the drive answered with something that is not a release manifest.')
        }
        if ($manifest.product -ne 'olsyn-drive' -or $manifest.channel -ne $Channel) {
            return (Fail 'the release manifest is for another product or channel.')
        }
        $version = [string]$manifest.version

        # --- 2. The installer for this PC ---------------------------------------
        $artifact = $manifest.artifacts | Where-Object { $_.platform -eq $platform -and $_.role -eq 'installer' } | Select-Object -First 1
        if (-not $artifact -and $platform -eq 'windows-arm64') {
            $artifact = $manifest.artifacts | Where-Object { $_.platform -eq 'windows-x64' -and $_.role -eq 'installer' } | Select-Object -First 1
            if ($artifact) { Say 'There is no Arm build of this release yet; installing the x64 build, which Windows runs on Arm too.' }
        }
        if (-not $artifact) { return (Fail "release $version has no installer for $platform.") }
        if ([string]$artifact.file -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$') { return (Fail 'the release manifest names an unexpected file.') }

        # Already installed at this version?
        $installed = ''
        try {
            $installed = (Get-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\OlsynDrive' -Name DisplayVersion -ErrorAction Stop).DisplayVersion
        } catch { }
        $exeInstalled = Join-Path $env:LOCALAPPDATA 'Programs\Olsyn Drive\olsyn-drive.exe'
        if (-not $Force -and $installed -eq $version -and (Test-Path -LiteralPath $exeInstalled)) {
            Say "Olsyn Drive $version is already installed."
            if (-not $NoLaunch) { Start-Process -FilePath $exeInstalled }
            return 0
        }

        # --- 3. Download, size and SHA-256 --------------------------------------
        $url = [string]$artifact.url
        if ($url -notmatch '^https?://') { $url = $Server + $url }
        $exe = Join-Path $temp ([string]$artifact.file)
        Say ("Downloading Olsyn Drive $version for $platform ({0:N1} MB)" -f ([double]$artifact.size / 1MB))
        try {
            Invoke-WebRequest -Uri $url -OutFile $exe -UseBasicParsing # follows the drive's redirect to storage
        } catch {
            return (Fail ('the download failed. (' + $_.Exception.Message + ')'))
        }
        $size = (Get-Item -LiteralPath $exe).Length
        if ($size -ne [long]$artifact.size) { return (Fail "the download is $size bytes; the manifest says $($artifact.size).") }
        $sha = (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($sha -ne ([string]$artifact.sha256).ToLowerInvariant()) { return (Fail 'the download does not match the SHA-256 in the manifest.') }
        Say 'Checked: size and SHA-256 match the release manifest.'

        # --- 4. Authenticode ---------------------------------------------------
        $sig = Get-AuthenticodeSignature -LiteralPath $exe
        switch ([string]$sig.Status) {
            'Valid' { Say ('Signed by: ' + $sig.SignerCertificate.Subject) }
            'NotSigned' { Say 'Note: this build is not signed. Its size and SHA-256 were checked against the release manifest, and the installer checks the manifest''s signature next.' }
            default { return (Fail ('the file is signed, but its signature does not check out (' + $sig.Status + ': ' + $sig.StatusMessage + ').')) }
        }

        # --- 5. Install (the installer checks the manifest's signature) ----------
        $installArgs = @('install', '--user', '--quiet', '--manifest', ('"' + $manifestFile + '"'))
        if ($AllowUnverified) { $installArgs += '--allow-unverified' }
        Say 'Installing.'
        # Not -Wait: Windows PowerShell would also wait for the Olsyn Drive the installer starts.
        $p = Start-Process -FilePath $exe -ArgumentList $installArgs -PassThru -NoNewWindow
        $null = $p.Handle # keeps the exit code readable after the process ends
        $p.WaitForExit()
        if ($p.ExitCode -ne 0) { return (Fail ("the installer stopped with code " + $p.ExitCode + '. Its message is above.')) }

        Say "Olsyn Drive $version is installed."
        if (-not $NoLaunch -and (Test-Path -LiteralPath $exeInstalled)) {
            Say 'Opening Olsyn Drive: sign in there to connect the drive.'
            Start-Process -FilePath $exeInstalled
        }
        return 0
    } catch {
        return (Fail $_.Exception.Message)
    } finally {
        $ProgressPreference = $oldProgress
        if (Test-Path -LiteralPath $temp) { Remove-Item -LiteralPath $temp -Recurse -Force -ErrorAction SilentlyContinue }
    }
}

$code = Install-OlsynDrive @args
$global:LASTEXITCODE = $code
# Run as a file: end with the code. Piped into iex: leave the window open.
if ($MyInvocation.MyCommand.Path) { exit $code }
