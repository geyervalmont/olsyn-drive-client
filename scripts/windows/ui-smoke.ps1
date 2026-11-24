<#
.SYNOPSIS
  Smoke test of the desktop app's agent side, for the desktop session of a
  Windows test machine. Synthetic data only; everything lives under %TEMP%.

.DESCRIPTION
  Starts fakedrive-server (with a data folder, NOT --auto-approve), starts
  olsyn-drive.exe with no arguments (the desktop app, which starts the agent)
  with a fresh OLSYN_DRIVE_HOME, then drives the agent over its named pipe
  the way the app does, speaking HTTP/1.1 from PowerShell:

    GET /v1/state, POST /v1/signin, POST the fake drive's /approve,
    wait for connected (and mounted when WinFsp is installed),
    GET /v1/transfers, then checks ui.log and agent.log.

  Each step writes "PASS  name" or "FAIL  name: detail" to the result file
  (and the console); screenshots of the whole screen are saved next to it so
  what the window showed can be looked at afterwards. Exit code: the number
  of failed steps.

  Windows PowerShell 5.1 compatible.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File ui-smoke.ps1 -Bin C:\olsyn\dev
.EXAMPLE
  powershell -ExecutionPolicy Bypass -File ui-smoke.ps1 -Screenshot C:\temp\screen.png
#>
[CmdletBinding(DefaultParameterSetName = 'Run')]
param(
  # Folder holding olsyn-drive.exe and fakedrive-server.exe (make dev: dist\windows-amd64\dev).
  [Parameter(ParameterSetName = 'Run', Mandatory = $true)] [string] $Bin,
  # Where the PASS/FAIL lines go; screenshots are written to the same folder.
  [Parameter(ParameterSetName = 'Run')] [string] $Result = (Join-Path $env:TEMP 'olsyn-ui-smoke\result.txt'),
  # Leave the app, the agent and the fake drive running at the end (to click around).
  [Parameter(ParameterSetName = 'Run')] [switch] $KeepRunning,
  # Only take a full-screen PNG screenshot to this path, then exit.
  [Parameter(ParameterSetName = 'Shot', Mandatory = $true)] [string] $Screenshot
)

Set-StrictMode -Version 1.0
$ErrorActionPreference = 'Stop'

# ---- screenshot helper ------------------------------------------------------

function Save-Screenshot {
  <# Saves the whole virtual screen (every monitor) as a PNG at $Path. #>
  param([Parameter(Mandatory = $true)] [string] $Path)
  Add-Type -AssemblyName System.Windows.Forms, System.Drawing
  if (-not ('OlsynSmoke.Dpi' -as [type])) {
    # Without this, a scaled display is captured at its unscaled size.
    Add-Type -Namespace OlsynSmoke -Name Dpi -MemberDefinition '[DllImport("user32.dll")] public static extern bool SetProcessDPIAware();'
  }
  [void][OlsynSmoke.Dpi]::SetProcessDPIAware()
  $bounds = [System.Windows.Forms.SystemInformation]::VirtualScreen
  $bmp = New-Object System.Drawing.Bitmap $bounds.Width, $bounds.Height
  try {
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    try {
      $g.CopyFromScreen($bounds.Left, $bounds.Top, 0, 0, $bmp.Size)
    } finally { $g.Dispose() }
    $dir = Split-Path -Parent $Path
    if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir | Out-Null }
    $bmp.Save($Path, [System.Drawing.Imaging.ImageFormat]::Png)
  } finally { $bmp.Dispose() }
}

if ($PSCmdlet.ParameterSetName -eq 'Shot') {
  Save-Screenshot -Path $Screenshot
  Write-Output "saved $Screenshot"
  exit 0
}

# ---- the agent's pipe, spoken to like the app does ----------------------------

$Sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$PipeName = "olsyn-drive-$Sid"

function Read-Some {
  param($Stream, [byte[]] $Buffer, [int] $Offset, [int] $Count, [datetime] $Deadline)
  $left = [int]($Deadline - (Get-Date)).TotalMilliseconds
  if ($left -le 0) { throw "the agent did not answer in time" }
  $task = $Stream.ReadAsync($Buffer, $Offset, $Count)
  if (-not $task.Wait($left)) { throw "the agent did not answer in time" }
  return $task.Result
}

function Invoke-Agent {
  <#
    One HTTP/1.1 request over the agent's named pipe. Returns an object with
    Status (int), Body (string) and Json (parsed, or $null).
  #>
  param(
    [Parameter(Mandatory = $true)] [string] $Method,
    [Parameter(Mandatory = $true)] [string] $Path,
    [string] $Body = '',
    [int] $TimeoutSeconds = 15
  )
  $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
  $pipe = New-Object System.IO.Pipes.NamedPipeClientStream('.', $PipeName, [System.IO.Pipes.PipeDirection]::InOut, [System.IO.Pipes.PipeOptions]::Asynchronous)
  try {
    $pipe.Connect([Math]::Min(5000, $TimeoutSeconds * 1000))
    $payload = [System.Text.Encoding]::UTF8.GetBytes($Body)
    $head = "$Method $Path HTTP/1.1`r`nHost: olsyn-drive`r`nUser-Agent: olsyn-ui-smoke`r`nAccept: application/json`r`nConnection: close`r`n"
    if ($payload.Length -gt 0) { $head += "Content-Type: application/json`r`n" }
    $head += "Content-Length: $($payload.Length)`r`n`r`n"
    $hb = [System.Text.Encoding]::ASCII.GetBytes($head)
    $pipe.Write($hb, 0, $hb.Length)
    if ($payload.Length -gt 0) { $pipe.Write($payload, 0, $payload.Length) }
    $pipe.Flush()

    # Everything until the agent closes the connection (Connection: close).
    $all = New-Object System.IO.MemoryStream
    $buf = New-Object byte[] 65536
    while ($true) {
      $n = Read-Some $pipe $buf 0 $buf.Length $deadline
      if ($n -le 0) { break }
      $all.Write($buf, 0, $n)
    }
    $bytes = $all.ToArray()
  } finally {
    $pipe.Dispose()
  }

  # Parsed as Latin-1 (one char per byte, so chunk sizes in bytes line up),
  # the body is then decoded as UTF-8.
  $latin1 = [System.Text.Encoding]::GetEncoding(28591)
  $text = $latin1.GetString($bytes)
  $split = $text.IndexOf("`r`n`r`n")
  if ($split -lt 0) { throw "not an HTTP response" }
  $headers = $text.Substring(0, $split) -split "`r`n"
  $status = [int](($headers[0] -split ' ')[1])
  $raw = $text.Substring($split + 4)
  if ($headers -match '^Transfer-Encoding:\s*chunked') {
    $out = New-Object System.Text.StringBuilder
    $rest = $raw
    while ($true) {
      $eol = $rest.IndexOf("`r`n")
      if ($eol -lt 0) { break }
      $size = [Convert]::ToInt32(($rest.Substring(0, $eol) -split ';')[0].Trim(), 16)
      if ($size -eq 0) { break }
      [void]$out.Append($rest.Substring($eol + 2, $size))
      $rest = $rest.Substring($eol + 2 + $size + 2)
    }
    $raw = $out.ToString()
  }
  $bodyText = [System.Text.Encoding]::UTF8.GetString($latin1.GetBytes($raw))
  $json = $null
  if ($bodyText.Trim().StartsWith('{')) { $json = $bodyText | ConvertFrom-Json }
  return New-Object psobject -Property @{ Status = $status; Body = $bodyText; Json = $json }
}

function Wait-Until {
  <# Runs $Test every half second until it returns $true or $Seconds pass. #>
  param([scriptblock] $Test, [int] $Seconds)
  $deadline = (Get-Date).AddSeconds($Seconds)
  while ((Get-Date) -lt $deadline) {
    try { if (& $Test) { return $true } } catch { }
    Start-Sleep -Milliseconds 500
  }
  return $false
}

# ---- results ------------------------------------------------------------------

$ResultDir = Split-Path -Parent $Result
if (-not (Test-Path $ResultDir)) { New-Item -ItemType Directory -Path $ResultDir | Out-Null }
Set-Content -Path $Result -Value "olsyn-drive ui smoke $(Get-Date -Format s)" -Encoding UTF8
$script:Fails = 0

function Step {
  param([string] $Name, [bool] $Ok, [string] $Detail = '')
  if ($Ok) { $line = "PASS  $Name" } else { $line = "FAIL  $Name"; if ($Detail) { $line += ": $Detail" }; $script:Fails++ }
  Add-Content -Path $Result -Value $line -Encoding UTF8
  Write-Output $line
}

function Shot {
  param([string] $Name)
  try { Save-Screenshot -Path (Join-Path $ResultDir "$Name.png") } catch { Add-Content -Path $Result -Value "NOTE  screenshot $Name failed: $_" }
}

# ---- run ------------------------------------------------------------------------

$Od = Join-Path (Resolve-Path $Bin) 'olsyn-drive.exe'
$Fd = Join-Path (Resolve-Path $Bin) 'fakedrive-server.exe'
foreach ($f in @($Od, $Fd)) { if (-not (Test-Path $f)) { throw "missing $f" } }

$Work = Join-Path $env:TEMP 'olsyn-ui-smoke-work'
if (Test-Path $Work) { Remove-Item -Recurse -Force $Work }
New-Item -ItemType Directory -Path (Join-Path $Work 'fd-data') | Out-Null
$TestHome = Join-Path $Work 'home'
$Server = 'http://127.0.0.1:8787'
$Faults = 'http://127.0.0.1:8788'

# The app and the agent it starts inherit these (a dev build honours both).
$env:OLSYN_DRIVE_HOME = $TestHome
$env:OLSYN_DRIVE_SERVER = $Server
Remove-Item Env:\OLSYN_DRIVE_TOKEN_STORE -ErrorAction SilentlyContinue

$fdProc = $null
$appProc = $null
try {
  # Windows PowerShell 5.1 does not quote -ArgumentList items: quote the path.
  $fdData = Join-Path $Work 'fd-data'
  $fdProc = Start-Process -FilePath $Fd -ArgumentList "--data `"$fdData`"" -WindowStyle Minimized -PassThru
  $ok = Wait-Until -Seconds 15 -Test { (Invoke-WebRequest -UseBasicParsing -Uri "$Faults/stats" -TimeoutSec 2).StatusCode -eq 200 }
  Step 'fake drive answers' $ok
  if (-not $ok) { throw 'the fake drive did not start' }

  $appProc = Start-Process -FilePath $Od -PassThru
  $script:state = $null
  $ok = Wait-Until -Seconds 30 -Test { $r = Invoke-Agent GET '/v1/state' -TimeoutSeconds 3; $script:state = $r.Json; $r.Status -eq 200 }
  Step 'the app started the agent and its pipe answers GET /v1/state' $ok
  if (-not $ok) { throw 'the agent never answered on its pipe' }
  Start-Sleep -Seconds 2
  Shot '1-app-started'

  Step 'a fresh home is signed out' ($script:state.connection -eq 'signed_out') "connection is $($script:state.connection)"

  $r = Invoke-Agent POST '/v1/signin' '{"open_browser":false}'
  $code = ''
  if ($r.Json -and ($r.Json.PSObject.Properties.Name -contains 'user_code')) { $code = $r.Json.user_code }
  Step 'POST /v1/signin gives a code' (($r.Status -eq 202) -and ($code -ne '')) "status $($r.Status) $($r.Body)"

  $r = Invoke-Agent GET '/v1/state'
  $linking = ($r.Json.connection -eq 'linking') -and ($r.Json.signin -ne $null) -and ($r.Json.signin.state -eq 'waiting')
  Step 'the state shows linking and a waiting code' $linking "connection $($r.Json.connection)"
  Start-Sleep -Seconds 1
  Shot '2-code-waiting'

  $ap = Invoke-WebRequest -UseBasicParsing -Method Post -Uri "$Faults/approve" -TimeoutSec 5
  Step 'the fake drive approves the sign-in' ($ap.StatusCode -eq 200)

  $ok = Wait-Until -Seconds 30 -Test { $r = Invoke-Agent GET '/v1/state' -TimeoutSeconds 3; $script:state = $r.Json; ($r.Json.account -ne $null) -and (@('connected', 'working') -contains $r.Json.connection) }
  Step 'signed in and connected' $ok "connection $($script:state.connection)"

  if (Test-Path (Join-Path ${env:ProgramFiles(x86)} 'WinFsp\bin\winfsp-x64.dll')) {
    $ok = Wait-Until -Seconds 45 -Test { $r = Invoke-Agent GET '/v1/state' -TimeoutSeconds 3; $script:state = $r.Json; $r.Json.mount.state -eq 'mounted' }
    Step 'the drive is mounted' $ok "mount $($script:state.mount.state)"
  } else {
    Add-Content -Path $Result -Value 'SKIP  the drive is mounted: WinFsp is not installed'
    Write-Output 'SKIP  the drive is mounted: WinFsp is not installed'
  }
  Start-Sleep -Seconds 2
  Shot '3-signed-in'

  $r = Invoke-Agent GET '/v1/transfers?state=active&limit=20'
  $hasItems = ($r.Json -ne $null) -and ($r.Json.PSObject.Properties.Name -contains 'items')
  Step 'GET /v1/transfers answers with a list' (($r.Status -eq 200) -and $hasItems) "status $($r.Status)"

  $uiLog = Join-Path $TestHome 'logs\ui.log'
  $agentLog = Join-Path $TestHome 'logs\agent.log'
  Step 'ui.log exists' (Test-Path $uiLog)
  if (Test-Path $uiLog) {
    $ui = Get-Content -Raw $uiLog
    Step 'ui.log has no per-asset Wails lines' (-not ($ui -match 'Handling request'))
    Step 'ui.log has no failed page or bridge requests' (-not ($ui -match 'page request failed|agent request failed')) 'see ui.log'
  }
  $tokenHit = Select-String -Path (Join-Path $TestHome 'logs\*') -Pattern 'odt_' -SimpleMatch -List -ErrorAction SilentlyContinue
  Step 'no token in the logs' ($tokenHit -eq $null)
  Step 'agent.log exists' (Test-Path $agentLog)
}
catch {
  Step 'smoke run' $false "$_"
  Shot 'error'
}
finally {
  if (-not $KeepRunning) {
    try { [void](Invoke-Agent POST '/v1/quit' '{}' -TimeoutSeconds 5) } catch { }
    if ($appProc -and -not $appProc.HasExited) { Stop-Process -Id $appProc.Id -Force -ErrorAction SilentlyContinue }
    if ($fdProc -and -not $fdProc.HasExited) { Stop-Process -Id $fdProc.Id -Force -ErrorAction SilentlyContinue }
  } else {
    Write-Output "left running: the app, the agent and the fake drive (home $TestHome)"
  }
}

if ($script:Fails -eq 0) { $summary = 'All steps passed.' } else { $summary = "$($script:Fails) step(s) failed." }
Add-Content -Path $Result -Value $summary -Encoding UTF8
Write-Output $summary
Write-Output "results: $Result"
exit $script:Fails
