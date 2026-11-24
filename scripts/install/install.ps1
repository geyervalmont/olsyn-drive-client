# Olsyn Drive installer for Windows (per user).
#
#   irm https://drive.olsyn.com/install.ps1 | iex
#   & ([scriptblock]::Create((irm https://drive.olsyn.com/install.ps1))) [-PreRelease] [-Installer <exe> [-Sha256 <hex>]] [-DryRun]
#   powershell -ExecutionPolicy Bypass -File install.ps1 [-Server <url>] [-Channel stable|beta] [-NoLaunch] [-Force]
#   powershell -File install.ps1 -SelfTest
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
# Pre-release (unsigned) builds. With -PreRelease, or by itself when the
# installer is not Authenticode-signed, it first walks the person through
# allowing the program folder (%LOCALAPPDATA%\Programs\Olsyn Drive) in
# Windows Security, which Microsoft Defender needs for an unsigned build:
# it creates the folder, copies its path, opens Windows Security and checks
# afterwards. It NEVER changes a Windows Security or Defender setting
# itself (the person does, by hand); it only reads the exclusion list, and
# only when the window already runs as administrator. It never asks for
# administrator rights. Without them it cannot read the list and asks the
# person to confirm instead.
#   -Installer <exe>  install a file handed over directly instead of reading the release manifest
#                     (it prints the file's SHA-256; with -Sha256 <hex> it refuses a file that does not match)
#   -DryRun           walk through the steps without downloading or installing anything
#   -SelfTest         check this script's own logic (no network, no changes); exit 0 when it passes
#
# The script does NOT check the manifest's Ed25519 signature: Windows
# PowerShell 5.1 has no Ed25519. It passes the manifest, exactly as the
# drive served it, to the installer, and the installer checks the signature
# with the keys compiled into it and refuses to install when it does not
# check out, or when the downloaded file is not the one the signed manifest
# names. Running it again updates an older installed copy. Nothing is sent
# anywhere except the requests above.
#
# Exit codes: 0 installed (or already up to date), 1 failed, 2 not supported on this PC,
# 3 stopped at the person's request (nothing installed).
# When piped into iex the exit code is left in $LASTEXITCODE and the window stays open.

# The drive server fills this in when it serves the script; the default is used when it did not.
$DriveBase = '__DRIVE_BASE__'

# --- Pure helpers (no side effects; -SelfTest runs them) ----------------------

# ConvertTo-OlsynComparablePath turns a folder path into the form used to
# compare two of them: quotes and spaces around it removed, / as \, doubled
# separators collapsed, no trailing separator (except on a drive root such as
# c:\), no \\?\ prefix, lower case. Environment variables are not expanded.
function ConvertTo-OlsynComparablePath {
    param([string]$Path)
    if (-not $Path) { return '' }
    $p = $Path.Trim().Trim('"').Trim()
    $p = $p.Replace('/', '\')
    if ($p.StartsWith('\\?\UNC\', [StringComparison]::OrdinalIgnoreCase)) { $p = '\\' + $p.Substring(8) }
    elseif ($p.StartsWith('\\?\')) { $p = $p.Substring(4) }
    $unc = $p.StartsWith('\\')
    while ($p.Contains('\\')) { $p = $p.Replace('\\', '\') }
    if ($unc) { $p = '\' + $p }
    if ($p -notmatch '^[A-Za-z]:\\$') { $p = $p.TrimEnd('\') }
    return $p.ToLowerInvariant()
}

# Test-OlsynExclusion says how a list of Windows Security folder exclusions
# covers Folder: 'exact' when one of them is that folder (any case, with or
# without a trailing \, or a * / ? pattern matching it), 'broader' when one is
# a folder above it, 'none' otherwise.
function Test-OlsynExclusion {
    param([string[]]$Exclusions, [string]$Folder)
    $target = ConvertTo-OlsynComparablePath $Folder
    if (-not $target) { return 'none' }
    # The folders above the target, nearest first: ...\programs, ..., c:\
    $above = New-Object System.Collections.Generic.List[string]
    $a = $target
    while ($true) {
        $i = $a.LastIndexOf('\')
        if ($i -le 0) { break }
        $a = $a.Substring(0, $i)
        if ($a -match '^[a-z]:$') { $above.Add($a + '\'); break }
        if ($a -eq '\') { break }
        $above.Add($a)
    }
    $tick = [string][char]96 # the PowerShell escape character, so [ and ] in a path are not pattern ranges
    $found = 'none'
    foreach ($entry in @($Exclusions)) {
        $e = ConvertTo-OlsynComparablePath $entry
        if (-not $e) { continue }
        if ($e.IndexOfAny([char[]]'*?') -ge 0) {
            $escaped = $e.Replace('[', $tick + '[').Replace(']', $tick + ']')
            $pattern = New-Object System.Management.Automation.WildcardPattern -ArgumentList $escaped, ([System.Management.Automation.WildcardOptions]::IgnoreCase)
            if ($pattern.IsMatch($target)) { return 'exact' }
            foreach ($up in $above) { if ($pattern.IsMatch($up)) { $found = 'broader' } }
        } else {
            if ($e -eq $target) { return 'exact' }
            if ($above.Contains($e)) { $found = 'broader' }
        }
    }
    return $found
}

# Select-OlsynArtifact picks the installer for a platform from a release
# manifest's artifacts. An Arm PC without an Arm build gets the x64 build,
# which Windows runs on Arm too (Fallback is then true). $null when there is
# none.
function Select-OlsynArtifact {
    param([object[]]$Artifacts, [string]$Platform)
    $pick = @($Artifacts) | Where-Object { $_ -and $_.platform -eq $Platform -and $_.role -eq 'installer' } | Select-Object -First 1
    if ($pick) { return [pscustomobject]@{ Artifact = $pick; Fallback = $false } }
    if ($Platform -eq 'windows-arm64') {
        $pick = @($Artifacts) | Where-Object { $_ -and $_.platform -eq 'windows-x64' -and $_.role -eq 'installer' } | Select-Object -First 1
        if ($pick) { return [pscustomobject]@{ Artifact = $pick; Fallback = $true } }
    }
    return $null
}

# Test-OlsynArtifactFileName: the manifest's file name is a plain name, never a path.
function Test-OlsynArtifactFileName {
    param([string]$Name)
    return ($Name -match '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$') -and ($Name -notmatch '\.\.')
}

# Invoke-OlsynSelfTest runs the pure helpers against known answers and
# returns 0 when all pass, 1 otherwise. It needs no network and changes
# nothing, so it runs on any PC (PowerShell 7 on macOS or Linux too).
function Invoke-OlsynSelfTest {
    $results = New-Object System.Collections.Generic.List[bool]
    function Check([string]$name, $got, $want) {
        if ([string]$got -ceq [string]$want) { Write-Host ('ok    ' + $name); $results.Add($true) }
        else { Write-Host ('FAIL  ' + $name + ": got '" + $got + "', want '" + $want + "'") -ForegroundColor Red; $results.Add($false) }
    }

    $dir = 'C:\Users\Test User\AppData\Local\Programs\Olsyn Drive'
    $norm = 'c:\users\test user\appdata\local\programs\olsyn drive'
    Check 'path: lower case, no trailing \' (ConvertTo-OlsynComparablePath ($dir + '\')) $norm
    Check 'path: quotes, spaces, / and //' (ConvertTo-OlsynComparablePath '  "C:/Users/Test User//AppData/Local/Programs/Olsyn Drive/"  ') $norm
    Check 'path: drive root keeps its \' (ConvertTo-OlsynComparablePath 'C:\') 'c:\'
    Check 'path: UNC' (ConvertTo-OlsynComparablePath '\\Server\Share\Dir\') '\\server\share\dir'
    Check 'path: \\?\ prefix' (ConvertTo-OlsynComparablePath '\\?\C:\Dir\') 'c:\dir'
    Check 'path: \\?\UNC\ prefix' (ConvertTo-OlsynComparablePath '\\?\UNC\server\share') '\\server\share'
    Check 'path: empty' (ConvertTo-OlsynComparablePath '') ''

    Check 'exclusion: same folder, other case, trailing \' (Test-OlsynExclusion @('c:\users\TEST USER\appdata\local\programs\olsyn drive\') $dir) 'exact'
    Check 'exclusion: among others' (Test-OlsynExclusion @('D:\Media', $dir, 'E:\') $dir) 'exact'
    Check 'exclusion: wildcard for every user' (Test-OlsynExclusion @('C:\Users\*\AppData\Local\Programs\Olsyn Drive') $dir) 'exact'
    Check 'exclusion: the folder above' (Test-OlsynExclusion @('C:\Users\Test User\AppData\Local\Programs\') $dir) 'broader'
    Check 'exclusion: whole disk' (Test-OlsynExclusion @('C:\') $dir) 'broader'
    Check 'exclusion: wildcard above' (Test-OlsynExclusion @('C:\Users\*\AppData\Local') $dir) 'broader'
    Check 'exclusion: name that only starts the same' (Test-OlsynExclusion @($dir + ' Old', $dir + '2') $dir) 'none'
    Check 'exclusion: a folder inside it' (Test-OlsynExclusion @($dir + '\bin') $dir) 'none'
    Check 'exclusion: another disk' (Test-OlsynExclusion @('D:\Users\Test User\AppData\Local\Programs\Olsyn Drive') $dir) 'none'
    Check 'exclusion: not readable without admin' (Test-OlsynExclusion @('N/A: Must be an administrator to view exclusions') $dir) 'none'
    Check 'exclusion: brackets are not a pattern' (Test-OlsynExclusion @('C:\Users\Test User\AppData\Local\Programs\Olsyn Driv[e]') $dir) 'none'
    Check 'exclusion: empty list' (Test-OlsynExclusion @() $dir) 'none'
    Check 'exclusion: no list' (Test-OlsynExclusion $null $dir) 'none'

    $x64 = [pscustomobject]@{ platform = 'windows-x64'; role = 'installer'; file = 'olsyn-drive.exe' }
    $x64u = [pscustomobject]@{ platform = 'windows-x64'; role = 'update'; file = 'olsyn-drive-update.exe' }
    $arm = [pscustomobject]@{ platform = 'windows-arm64'; role = 'installer'; file = 'olsyn-drive-arm64.exe' }
    $pick = Select-OlsynArtifact @($x64u, $x64, $arm) 'windows-x64'
    Check 'artifact: x64 installer, not the update' ($pick.Artifact.file) 'olsyn-drive.exe'
    Check 'artifact: x64 is not a fallback' ($pick.Fallback) 'False'
    $pick = Select-OlsynArtifact @($x64, $arm) 'windows-arm64'
    Check 'artifact: Arm build on Arm' ($pick.Artifact.file) 'olsyn-drive-arm64.exe'
    $pick = Select-OlsynArtifact @($x64u, $x64) 'windows-arm64'
    Check 'artifact: x64 build on Arm when there is no Arm build' ($pick.Artifact.file) 'olsyn-drive.exe'
    Check 'artifact: ... marked as fallback' ($pick.Fallback) 'True'
    Check 'artifact: none for x64 when only Arm' ($null -eq (Select-OlsynArtifact @($arm) 'windows-x64')) 'True'
    Check 'artifact: none in an empty manifest' ($null -eq (Select-OlsynArtifact @() 'windows-x64')) 'True'

    Check 'file name: plain' (Test-OlsynArtifactFileName 'olsyn-drive-1.2.3.exe') 'True'
    Check 'file name: path' (Test-OlsynArtifactFileName '..\olsyn-drive.exe') 'False'
    Check 'file name: slash' (Test-OlsynArtifactFileName 'a/b.exe') 'False'
    Check 'file name: dots' (Test-OlsynArtifactFileName 'a..exe') 'False'

    $bad = @($results | Where-Object { -not $_ }).Count
    if ($bad -gt 0) {
        Write-Host ('Self-test FAILED: ' + $bad + ' of ' + $results.Count + ' checks.') -ForegroundColor Red
        return 1
    }
    Write-Host ('Self-test passed: ' + $results.Count + ' checks.') -ForegroundColor Green
    return 0
}

# --- Windows helpers (they read; none of them changes a setting) --------------

# Get-OlsynSecurityState reads whether Microsoft Defender is the antivirus in
# charge, and the names of any other antivirus Windows knows about.
function Get-OlsynSecurityState {
    $state = [pscustomobject]@{ DefenderActive = $false; Others = @() }
    try {
        $s = Get-MpComputerStatus -ErrorAction Stop
        $passive = ([string]$s.AMRunningMode) -match 'Passive|EDR|Not running'
        $state.DefenderActive = [bool]$s.AMServiceEnabled -and ($s.AntivirusEnabled -ne $false) -and -not $passive
    } catch { }
    try {
        $state.Others = @(Get-CimInstance -Namespace 'root/SecurityCenter2' -ClassName AntiVirusProduct -ErrorAction Stop |
            ForEach-Object { [string]$_.displayName } | Where-Object { $_ -and $_ -notmatch 'Defender' } | Select-Object -Unique)
    } catch { }
    return $state
}

# Get-OlsynExclusionList reads Defender's folder exclusions. Windows only
# shows them to an administrator; otherwise Readable is false.
function Get-OlsynExclusionList {
    $list = [pscustomobject]@{ Readable = $false; Paths = @() }
    try { $pref = Get-MpPreference -ErrorAction Stop } catch { return $list }
    $paths = @($pref.ExclusionPath | Where-Object { $_ })
    if (@($paths | Where-Object { $_ -match '^N/A' }).Count -gt 0) { return $list }
    $list.Readable = $true
    $list.Paths = $paths
    return $list
}

# Open-OlsynWindowsSecurity opens Windows Security, on its Exclusions page
# when it can, and returns the address it opened ('' when none worked).
# Which of these Microsoft documents (checked 2026-11-24):
#   ms-settings:windowsdefender        documented: Microsoft Learn, "Launch Windows Settings",
#                                      ms-settings: URI reference (Settings > Windows Security)
#   windowsdefender:                   the Windows Security app's registered protocol; not on Learn
#   windowsdefender://exclusions       NOT documented by Microsoft (only Microsoft Q&A answers and
#   windowsdefender://threatsettings   third-party lists); they work on current Windows 10 and 11 but
#                                      may change, so the printed steps also say how to get there by hand.
# An unknown page inside a registered protocol just opens the app's home
# page, so the fallbacks only matter where the protocol itself is missing.
function Open-OlsynWindowsSecurity {
    foreach ($uri in @('windowsdefender://exclusions', 'windowsdefender://threatsettings', 'windowsdefender:', 'ms-settings:windowsdefender')) {
        try { Start-Process -FilePath $uri -ErrorAction Stop; return $uri } catch { }
    }
    return ''
}

# Test-OlsynSignedInAccount tells whether this window runs as the account
# signed in to the desktop ('same'), as another one, such as an IT account
# given at "Run as administrator" ('other'), or cannot tell ('unknown').
function Test-OlsynSignedInAccount {
    try {
        $me = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
        $owners = @(Get-CimInstance -ClassName Win32_Process -Filter "Name='explorer.exe'" -ErrorAction Stop |
            ForEach-Object { (Invoke-CimMethod -InputObject $_ -MethodName GetOwnerSid -ErrorAction Stop).Sid })
        if ($owners.Count -eq 0) { return 'unknown' }
        if ($owners -contains $me) { return 'same' }
        return 'other'
    } catch { return 'unknown' }
}

# Start-OlsynApp opens Olsyn Drive. From an administrator window it hands the
# program to the desktop (explorer.exe), which starts it with the person's
# normal rights: Olsyn Drive must not run as administrator, or its drive
# letter would not show in File Explorer.
function Start-OlsynApp {
    param([string]$Exe, [bool]$Elevated)
    try {
        if ($Elevated) { Start-Process -FilePath (Join-Path $env:WINDIR 'explorer.exe') -ArgumentList ('"' + $Exe + '"') -ErrorAction Stop }
        else { Start-Process -FilePath $Exe -ErrorAction Stop }
        return $true
    } catch { return $false }
}

function Write-OlsynStep {
    param([int]$Number, [string]$Title)
    Write-Host ''
    Write-Host ('Step ' + $Number + ' of 5: ' + $Title) -ForegroundColor Cyan
    Write-Host ('-' * (12 + $Title.Length)) -ForegroundColor Cyan
}

# Read-OlsynChoice asks until the answer starts with one of the letters
# offered and returns that letter in upper case.
function Read-OlsynChoice {
    param([string]$Prompt, [string[]]$Letters)
    while ($true) {
        $answer = ([string](Read-Host $Prompt)).Trim().ToUpperInvariant()
        if ($answer.Length -gt 0 -and $Letters -contains $answer.Substring(0, 1)) { return $answer.Substring(0, 1) }
        Write-Host ('Please type ' + ($Letters -join ' or ') + ', then press Enter.')
    }
}

function Show-OlsynExclusionClicks {
    param([string]$Folder)
    Write-Host ''
    Write-Host 'In Windows Security, on the Exclusions page:'
    Write-Host '   1. Click "Add an exclusion".'
    Write-Host '   2. Click "Folder".'
    Write-Host '   3. Click the address bar at the top of the folder window, paste the path (Ctrl+V),'
    Write-Host '      press Enter, then click "Select Folder".'
    Write-Host ''
    Write-Host 'If Windows asks "Do you want to allow this app to make changes to your device?", choose Yes.'
    Write-Host 'If Windows Security opened on another page, go to: Virus & threat protection >'
    Write-Host '"Manage settings" > Exclusions > "Add or remove exclusions".'
    Write-Host ('The path to paste is: ' + $Folder)
    Write-Host 'Add only this folder: never Downloads, the W: drive or a whole disk.'
}

# Steps 1 to 3: explain, have the person allow the program folder, check.
# Returns $true to go on, $false when the person chose to stop.
function Invoke-OlsynExclusionWizard {
    param([string]$Folder, [bool]$Elevated, [bool]$DryRun)

    Write-OlsynStep 1 'About this test version'
    Write-Host 'This is an unsigned test version (pre-release) of Olsyn Drive, so Windows does not know who made it yet.'
    Write-Host 'Windows Security has to be told, once, by you, that the Olsyn Drive program folder is safe.'

    Write-OlsynStep 2 'Allow the Olsyn Drive folder in Windows Security'
    if (Test-Path -LiteralPath $Folder) {
        Write-Host ('The folder is ready: ' + $Folder)
    } elseif ($DryRun) {
        Write-Host ('(Dry run: the folder would be created here: ' + $Folder + ')')
    } else {
        New-Item -ItemType Directory -Path $Folder -Force | Out-Null
        Write-Host ('Created the folder: ' + $Folder)
    }
    $security = Get-OlsynSecurityState
    $copy = {
        try { Set-Clipboard -Value $Folder -ErrorAction Stop; Write-Host 'Its path is copied: you can paste it with Ctrl+V.' }
        catch { Write-Host 'Its path could not be copied; select it above, then right-click to copy.' }
    }
    & $copy
    if (-not $security.DefenderActive) {
        $who = 'Another security program'
        if ($security.Others.Count -gt 0) { $who = ($security.Others -join ', ') }
        Write-Host ''
        Write-Host ($who + ' looks after this PC instead of Microsoft Defender.') -ForegroundColor Yellow
        Write-Host 'You may need to allow the folder above in that program (look for "exclusions", "exceptions" or'
        Write-Host '"allowed items" in its settings), or ask whoever looks after this PC to do it.'
    } else {
        if (Open-OlsynWindowsSecurity) { Write-Host 'Windows Security is now open (it may be behind this window).' }
        else { Write-Host 'Please open Windows Security from the Start menu.' }
        Show-OlsynExclusionClicks $Folder
    }

    while ($true) {
        Write-Host ''
        $null = Read-Host 'When you have done that, press Enter'
        Write-OlsynStep 3 'Check'
        $listed = $false
        $checked = $false
        if ($security.DefenderActive -and $Elevated) {
            $list = Get-OlsynExclusionList
            if ($list.Readable) {
                $checked = $true
                switch (Test-OlsynExclusion $list.Paths $Folder) {
                    'exact' { Write-Host 'Checked: Windows Security lists the Olsyn Drive folder.' -ForegroundColor Green; $listed = $true }
                    'broader' {
                        Write-Host 'Checked: a larger folder that contains it is allowed in Windows Security, so this works.' -ForegroundColor Green
                        Write-Host 'Only the Olsyn Drive folder is needed; you can narrow it down later.'
                        $listed = $true
                    }
                    default { Write-Host 'Windows Security does not list the Olsyn Drive folder yet.' -ForegroundColor Yellow }
                }
            } else {
                Write-Host 'Windows Security did not let this window read its list, so please check yourself.'
            }
        } elseif ($security.DefenderActive) {
            Write-Host 'This window is not running as administrator, so it cannot read the Windows Security list.'
            Write-Host 'Please look yourself: the Exclusions page should now show the Olsyn Drive folder.'
        }
        if (-not $checked) {
            $listed = (Read-OlsynChoice 'Is the Olsyn Drive folder allowed now? Type Y for yes or N for no' @('Y', 'N')) -eq 'Y'
        }
        if ($listed) { return $true }

        Write-Host ''
        Write-Host 'Without this, Windows Security may remove Olsyn Drive a few minutes after it is installed.'
        Show-OlsynExclusionClicks $Folder
        Write-Host ''
        $choice = Read-OlsynChoice 'Type R to try again, C to continue anyway, or Q to quit' @('R', 'C', 'Q')
        if ($choice -eq 'Q') { return $false }
        if ($choice -eq 'C') {
            Write-Host 'Continuing without it. If Windows Security removes Olsyn Drive, step 4 says what to do.'
            return $true
        }
        & $copy
        if ($security.DefenderActive) { $null = Open-OlsynWindowsSecurity }
    }
}

function Show-OlsynIfRemoved {
    Write-Host ''
    Write-Host 'If Windows Security still removes Olsyn Drive:'
    Write-Host '   - Open Windows Security > Virus & threat protection > "Protection history", select the'
    Write-Host '     Olsyn Drive item, then Actions > "Restore" (or "Allow on device").'
    Write-Host '   - Your files waiting to upload are safe. They are kept in your Olsyn Drive data folder, not in'
    Write-Host '     the program folder, and carry on uploading once Olsyn Drive is back. Do not delete that folder.'
}

function Show-OlsynRemoveLater {
    Write-OlsynStep 5 'Later: taking the folder out of Windows Security'
    Write-Host 'The signed release of Olsyn Drive will not need this. When you have it (or if you stop using'
    Write-Host 'Olsyn Drive), open Windows Security > Virus & threat protection > "Manage settings" > Exclusions >'
    Write-Host '"Add or remove exclusions", select the Olsyn Drive folder and choose "Remove".'
}

# Get-OlsynInstaller fetches and checks the installer into WorkDir: a local
# file (-Installer) or the one the release manifest names. Error is set
# when it failed; UpToDate when this version is already installed.
function Get-OlsynInstaller {
    param([string]$Server, [string]$Channel, [string]$Platform, [string]$Installer, [string]$Sha256,
        [string]$WorkDir, [string]$InstalledExe, [bool]$Force)
    $r = [pscustomobject]@{ Error = ''; Exe = ''; Sha256 = ''; Version = ''; ManifestFile = ''; UpToDate = $false }

    if ($Installer) {
        if (-not (Test-Path -LiteralPath $Installer -PathType Leaf)) { $r.Error = "there is no file at '$Installer'."; return $r }
        $src = (Resolve-Path -LiteralPath $Installer).ProviderPath
        $r.Exe = Join-Path $WorkDir 'olsyn-drive.exe'
        Copy-Item -LiteralPath $src -Destination $r.Exe -Force
        $item = Get-Item -LiteralPath $r.Exe
        $r.Version = [string]$item.VersionInfo.ProductVersion
        $r.Sha256 = (Get-FileHash -LiteralPath $r.Exe -Algorithm SHA256).Hash.ToLowerInvariant()
        Write-Host ('Installer: ' + $src)
        Write-Host ('  size:    {0:N0} bytes' -f $item.Length)
        Write-Host ('  SHA-256: ' + $r.Sha256)
        if ($Sha256) {
            if ($r.Sha256 -ne $Sha256.ToLowerInvariant()) { $r.Error = 'the file''s SHA-256 is not the one given with -Sha256: it is not the file you were sent.'; return $r }
            Write-Host 'Checked: the SHA-256 matches the one you were given.'
        } else {
            Write-Host 'No -Sha256 given: compare the SHA-256 above with the one sent to you with the file.'
        }
        return $r
    }

    # The release manifest.
    $manifestUrl = "$Server/api/client/v1/releases/$Channel.json"
    Write-Host "Looking for the latest Olsyn Drive ($Channel) on $Server"
    try {
        $resp = Invoke-WebRequest -Uri $manifestUrl -UseBasicParsing -Headers @{ Accept = 'application/json' }
    } catch {
        $r.Error = "could not read $manifestUrl. Check this PC is online. (" + $_.Exception.Message + ')'
        return $r
    }
    $raw = $resp.Content
    if ($raw -is [byte[]]) { $raw = [Text.Encoding]::UTF8.GetString($raw) }
    $r.ManifestFile = Join-Path $WorkDir "$Channel.json"
    [IO.File]::WriteAllText($r.ManifestFile, $raw, (New-Object Text.UTF8Encoding $false))
    try {
        $envelope = $raw | ConvertFrom-Json
        $manifestText = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($envelope.manifest))
        $manifest = $manifestText | ConvertFrom-Json
    } catch {
        $r.Error = 'the drive answered with something that is not a release manifest.'
        return $r
    }
    if ($manifest.product -ne 'olsyn-drive' -or $manifest.channel -ne $Channel) { $r.Error = 'the release manifest is for another product or channel.'; return $r }
    $r.Version = [string]$manifest.version

    # The installer for this PC.
    $pick = Select-OlsynArtifact @($manifest.artifacts) $Platform
    if (-not $pick) { $r.Error = "release $($r.Version) has no installer for $Platform."; return $r }
    if ($pick.Fallback) { Write-Host 'There is no Arm build of this release yet; installing the x64 build, which Windows runs on Arm too.' }
    $artifact = $pick.Artifact
    if (-not (Test-OlsynArtifactFileName ([string]$artifact.file))) { $r.Error = 'the release manifest names an unexpected file.'; return $r }

    # Already installed at this version?
    $installed = ''
    try {
        $installed = (Get-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\OlsynDrive' -Name DisplayVersion -ErrorAction Stop).DisplayVersion
    } catch { }
    if (-not $Force -and $installed -eq $r.Version -and (Test-Path -LiteralPath $InstalledExe)) {
        $r.UpToDate = $true
        return $r
    }

    # Download, size and SHA-256.
    $url = [string]$artifact.url
    if ($url -notmatch '^https?://') { $url = $Server + $url }
    $r.Exe = Join-Path $WorkDir ([string]$artifact.file)
    Write-Host ("Downloading Olsyn Drive $($r.Version) for $Platform ({0:N1} MB)" -f ([double]$artifact.size / 1MB))
    try {
        Invoke-WebRequest -Uri $url -OutFile $r.Exe -UseBasicParsing # follows the drive's redirect to storage
    } catch {
        $r.Error = 'the download failed. (' + $_.Exception.Message + ')'
        return $r
    }
    $size = (Get-Item -LiteralPath $r.Exe).Length
    if ($size -ne [long]$artifact.size) { $r.Error = "the download is $size bytes; the manifest says $($artifact.size)."; return $r }
    $r.Sha256 = (Get-FileHash -LiteralPath $r.Exe -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($r.Sha256 -ne ([string]$artifact.sha256).ToLowerInvariant()) { $r.Error = 'the download does not match the SHA-256 in the manifest.'; return $r }
    Write-Host 'Checked: size and SHA-256 match the release manifest.'
    return $r
}

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
        [switch]$AllowUnverified,
        # Unsigned pre-release: walk through allowing the program folder in Windows Security first.
        # (This also happens by itself when the installer turns out not to be signed.)
        [switch]$PreRelease,
        # Install this local file instead of the one the release manifest names.
        [string]$Installer = '',
        # With -Installer: the file's expected SHA-256 (64 hex digits).
        [string]$Sha256 = '',
        # Walk through the steps without downloading or installing anything.
        [switch]$DryRun,
        # Check this script's own logic and stop: 0 when it passes.
        [switch]$SelfTest
    )

    $ErrorActionPreference = 'Stop'
    if ($SelfTest) { return (Invoke-OlsynSelfTest) }
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
    if ($Sha256 -and -not $Installer) { return (Fail '-Sha256 goes with -Installer <file>.') }
    if ($Sha256 -and $Sha256 -notmatch '^[0-9A-Fa-f]{64}$') { return (Fail '-Sha256 must be 64 hexadecimal digits.') }

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
    $installDir = Join-Path $env:LOCALAPPDATA 'Programs\Olsyn Drive'
    $exeInstalled = Join-Path $installDir 'olsyn-drive.exe'
    $elevated = $false
    try {
        $id = [Security.Principal.WindowsIdentity]::GetCurrent()
        $elevated = (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    } catch { }
    # A signed release installs from a normal window. An administrator window
    # is fine for the pre-release steps (it lets them check Windows Security's
    # list), but only when it is the person's own account: the program folder
    # and settings are per account.
    if ($elevated -and (Test-OlsynSignedInAccount) -eq 'other') {
        return (Fail ('this administrator window runs as another Windows account than the one signed in to this PC, so Olsyn Drive would be installed for that account. ' +
                'Close it and run the same command in a normal PowerShell window (not as administrator): the steps are the same, and you confirm the Windows Security step yourself.'))
    }

    # --- Dry run: the steps, nothing downloaded or installed ---------------------
    if ($DryRun) {
        Say 'Dry run: nothing is downloaded or installed.'
        if ($Installer) { Say "It would install the file $Installer." }
        else { Say "It would read $Server/api/client/v1/releases/$Channel.json and download the installer for $platform." }
        if (-not (Invoke-OlsynExclusionWizard -Folder $installDir -Elevated $elevated -DryRun $true)) { Say 'Stopped.'; return 3 }
        Write-OlsynStep 4 'Download, check and install'
        Say '(Dry run: skipped. It would download or copy the installer, check its size and SHA-256, install it and open Olsyn Drive.)'
        Show-OlsynIfRemoved
        Show-OlsynRemoveLater
        return 0
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

    # Working folders, removed at the end. For a pre-release the installer is
    # kept in a folder inside the program folder, which Windows Security now
    # leaves alone; otherwise in the temporary folder.
    $workDirs = New-Object System.Collections.Generic.List[string]
    $newWorkDir = {
        param([string]$parent)
        $d = Join-Path $parent ('.olsyn-drive-install-' + [Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $d -Force | Out-Null
        $workDirs.Add($d)
        return $d
    }
    $wizard = [bool]$PreRelease
    try {
        if ($wizard) {
            if (-not (Invoke-OlsynExclusionWizard -Folder $installDir -Elevated $elevated -DryRun $false)) { Say 'Stopped: nothing was installed.'; return 3 }
            Write-OlsynStep 4 'Download, check and install'
            $work = & $newWorkDir $installDir
        } else {
            $work = & $newWorkDir ([IO.Path]::GetTempPath())
        }

        # --- The installer: size and SHA-256 ------------------------------------
        $got = Get-OlsynInstaller -Server $Server -Channel $Channel -Platform $platform -Installer $Installer -Sha256 $Sha256 -WorkDir $work -InstalledExe $exeInstalled -Force ([bool]$Force)
        if ($got.Error) { return (Fail $got.Error) }
        if ($got.UpToDate) {
            Say "Olsyn Drive $($got.Version) is already installed."
            if (-not $NoLaunch) { $null = Start-OlsynApp $exeInstalled $elevated }
            if ($wizard) { Show-OlsynIfRemoved; Show-OlsynRemoveLater }
            return 0
        }

        # --- Authenticode ------------------------------------------------------
        $sig = Get-AuthenticodeSignature -LiteralPath $got.Exe
        switch ([string]$sig.Status) {
            'Valid' { Say ('Signed by: ' + $sig.SignerCertificate.Subject) }
            'NotSigned' {
                if ($got.ManifestFile) { Say 'Note: this build is not signed. Its size and SHA-256 were checked against the release manifest, and the installer checks the manifest''s signature next.' }
                else { Say 'Note: this build is not signed.' }
            }
            default { return (Fail ('the file is signed, but its signature does not check out (' + $sig.Status + ': ' + $sig.StatusMessage + ').')) }
        }
        if (-not $wizard -and [string]$sig.Status -eq 'NotSigned') {
            Say 'Because it is not signed, Windows Security has to be told about it first. These steps show you how.'
            if (-not (Invoke-OlsynExclusionWizard -Folder $installDir -Elevated $elevated -DryRun $false)) { Say 'Stopped: nothing was installed.'; return 3 }
            $wizard = $true
            Write-OlsynStep 4 'Check and install'
            if (-not (Test-Path -LiteralPath $got.Exe)) { return (Fail 'the downloaded installer is gone: Windows Security probably removed it. Run this again now that the folder is allowed.') }
            # Move the installer (and manifest) into the allowed folder, then check it again.
            $work = & $newWorkDir $installDir
            $moved = Join-Path $work (Split-Path -Leaf $got.Exe)
            Copy-Item -LiteralPath $got.Exe -Destination $moved -Force
            if ($got.ManifestFile) {
                $movedManifest = Join-Path $work (Split-Path -Leaf $got.ManifestFile)
                Copy-Item -LiteralPath $got.ManifestFile -Destination $movedManifest -Force
                $got.ManifestFile = $movedManifest
            }
            $got.Exe = $moved
            if ((Get-FileHash -LiteralPath $got.Exe -Algorithm SHA256).Hash.ToLowerInvariant() -ne $got.Sha256) { return (Fail 'the installer changed while it was being moved.') }
            Say 'Checked again: SHA-256 unchanged.'
        }
        if (-not $wizard -and $elevated) {
            return (Fail 'this PowerShell window runs as administrator. Olsyn Drive installs for you only and needs no administrator rights: run the same command in a normal PowerShell window.')
        }

        # --- Install (the installer checks the manifest's signature) -------------
        if (-not (Test-Path -LiteralPath $got.Exe)) {
            return (Fail 'the installer file is gone: Windows Security probably removed it. Check the Windows Security step, then run this again.')
        }
        $installArgs = @('install', '--user', '--quiet')
        if ($got.ManifestFile) {
            $installArgs += @('--manifest', ('"' + $got.ManifestFile + '"'))
            if ($AllowUnverified) { $installArgs += '--allow-unverified' }
        }
        # From an administrator window the installer must not start Olsyn Drive
        # (it would run as administrator); it is opened below with normal rights.
        if ($elevated) { $installArgs += '--no-start' }
        Say 'Installing.'
        # Not -Wait: Windows PowerShell would also wait for the Olsyn Drive the installer starts.
        $p = Start-Process -FilePath $got.Exe -ArgumentList $installArgs -PassThru -NoNewWindow
        $null = $p.Handle # keeps the exit code readable after the process ends
        $p.WaitForExit()
        if ($p.ExitCode -ne 0) { return (Fail ("the installer stopped with code " + $p.ExitCode + '. Its message is above.')) }

        if ($got.Version) { Say "Olsyn Drive $($got.Version) is installed." } else { Say 'Olsyn Drive is installed.' }
        if (-not $NoLaunch -and (Test-Path -LiteralPath $exeInstalled)) {
            Say 'Opening Olsyn Drive: sign in there to connect the drive.'
            if (-not (Start-OlsynApp $exeInstalled $elevated)) { Say 'It did not open by itself: open Olsyn Drive from the Start menu.' }
        }
        if ($wizard) {
            Show-OlsynIfRemoved
            Show-OlsynRemoveLater
        }
        return 0
    } catch {
        return (Fail $_.Exception.Message)
    } finally {
        $ProgressPreference = $oldProgress
        foreach ($d in $workDirs) {
            if (Test-Path -LiteralPath $d) { Remove-Item -LiteralPath $d -Recurse -Force -ErrorAction SilentlyContinue }
        }
    }
}

$code = Install-OlsynDrive @args
$global:LASTEXITCODE = $code
# Run as a file: end with the code. Piped into iex: leave the window open.
if ($MyInvocation.MyCommand.Path) { exit $code }
