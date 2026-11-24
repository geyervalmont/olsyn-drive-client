<#
.SYNOPSIS
  Robustness checks of the Olsyn Drive client on a Windows test machine,
  run in the desktop session. Synthetic data only; work files live under
  %TEMP% and are removed at the end.

.DESCRIPTION
  Starts the agent of a dev build (olsyn-drive.exe agent, no desktop app)
  against a fake drive with a fresh OLSYN_DRIVE_HOME, signs in, mounts the
  drive, then performs and checks:

    a  500 files and one 1 GiB file copied onto the drive with uploads
       throttled to 16 MiB/s; the agent is killed (taskkill /f) while parts
       are still to send and restarted; the large file resumes from its
       finished parts and none is sent twice (the fake drive's part
       counters).
    b  the fake drive offline for 60 s during a copy: reading data that is
       not cached fails or answers within 2 s, writes go on into the cache,
       and the backlog drains afterwards.
    c  sign-out with unsent files is refused without confirmation, and the
       files stay intact.
    d  quit and restart with unsent files: they finish.
    e  a storm of 1,000 creates, overwrites, renames, deletes and new
       folders leaves the drive and the mounted copy identical.
    f  the tail of the fake drive's 5 GiB generated file is read, then 1 GiB
       sequentially, with the throughput printed.
    g  a document kept open by another process is saved the way Word does
       (save to a temp file, rename the original away, rename the temp file
       in, delete the original): the drive keeps one file with a new
       version, not a delete and a new file.

  Each check writes "PASS  name" or "FAIL  name: detail" (and NOTE lines
  with measurements) to the result file and the console. Exit code: the
  number of failed checks. Needs WinFsp: when it is missing the script asks
  the agent to install it (approve the Windows prompt).

  Windows PowerShell 5.1 compatible.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File robustness.ps1 -Bin C:\olsyn\dev
.EXAMPLE
  powershell -ExecutionPolicy Bypass -File robustness.ps1 -Bin C:\olsyn\dev -Server http://127.0.0.1:8787 -Result C:\temp\robustness.txt
#>
param(
  # Folder holding olsyn-drive.exe and fakedrive-server.exe (make dev: dist\windows-amd64\dev).
  [Parameter(Mandatory = $true)] [string] $Bin,
  # A fake drive that is already running; when omitted the script starts one.
  [string] $Server = '',
  # Its fault controls (default: the same host, the next port).
  [string] $Faults = '',
  # Where the PASS/FAIL lines go.
  [string] $Result = (Join-Path $env:TEMP 'olsyn-robustness\result.txt'),
  # The drive letter to ask for (a free one is used if it is taken).
  [string] $Letter = 'W:',
  # Leave the agent and the fake drive running at the end.
  [switch] $KeepRunning
)

Set-StrictMode -Version 1.0
$ErrorActionPreference = 'Stop'

# ---- results ------------------------------------------------------------------

$ResultDir = Split-Path -Parent $Result
if (-not (Test-Path $ResultDir)) { New-Item -ItemType Directory -Path $ResultDir | Out-Null }
$script:Fails = 0

function Write-Result {
  <# Writes one line to the result file. Another program reading the file at
     that moment (an editor, a log viewer, a test harness collecting it) can
     hold it briefly: the write is tried again for up to 10 s, sharing the
     file with readers, and a line that still cannot be written is shown on
     the console rather than failing the check that wrote it. #>
  param([string] $Line, [switch] $Create)
  $mode = [System.IO.FileMode]::Append
  if ($Create) { $mode = [System.IO.FileMode]::Create }
  $bytes = [System.Text.Encoding]::UTF8.GetBytes($Line + [Environment]::NewLine)
  $deadline = (Get-Date).AddSeconds(10)
  while ($true) {
    try {
      $f = [System.IO.File]::Open($Result, $mode, [System.IO.FileAccess]::Write, [System.IO.FileShare]::ReadWrite)
      try { $f.Write($bytes, 0, $bytes.Length) } finally { $f.Dispose() }
      return
    } catch [System.IO.IOException] {
      if ((Get-Date) -gt $deadline) {
        Write-Warning "could not write to $Result ($($_.Exception.Message)): $Line"
        return
      }
      Start-Sleep -Milliseconds 100
    }
  }
}

Write-Result "olsyn-drive robustness $(Get-Date -Format s)" -Create

function Step {
  param([string] $Name, [bool] $Ok, [string] $Detail = '')
  if ($Ok) { $line = "PASS  $Name" } else { $line = "FAIL  $Name"; $script:Fails++ }
  if ($Detail) { $line += ": $Detail" }
  Write-Result $line
  Write-Output $line
}

function Note {
  param([string] $Text)
  Write-Result "NOTE  $Text"
  Write-Output "NOTE  $Text"
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

# ---- the agent's pipe (HTTP/1.1, as the desktop app speaks it) --------------------

$Sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$PipeName = "olsyn-drive-$Sid"

function Read-Some {
  param($Stream, [byte[]] $Buffer, [int] $Offset, [int] $Count, [datetime] $Deadline)
  $left = [int]($Deadline - (Get-Date)).TotalMilliseconds
  if ($left -le 0) { throw 'the agent did not answer in time' }
  $task = $Stream.ReadAsync($Buffer, $Offset, $Count)
  if (-not $task.Wait($left)) { throw 'the agent did not answer in time' }
  return $task.Result
}

function Invoke-Agent {
  <# One request over the agent's named pipe: Status, Body and Json (or $null). #>
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
    $head = "$Method $Path HTTP/1.1`r`nHost: olsyn-drive`r`nUser-Agent: olsyn-robustness`r`nAccept: application/json`r`nConnection: close`r`n"
    if ($payload.Length -gt 0) { $head += "Content-Type: application/json`r`n" }
    $head += "Content-Length: $($payload.Length)`r`n`r`n"
    $hb = [System.Text.Encoding]::ASCII.GetBytes($head)
    $pipe.Write($hb, 0, $hb.Length)
    if ($payload.Length -gt 0) { $pipe.Write($payload, 0, $payload.Length) }
    $pipe.Flush()
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
  $latin1 = [System.Text.Encoding]::GetEncoding(28591)
  $text = $latin1.GetString($bytes)
  $split = $text.IndexOf("`r`n`r`n")
  if ($split -lt 0) { throw 'not an HTTP response' }
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

function Get-State {
  $r = Invoke-Agent GET '/v1/state' -TimeoutSeconds 5
  if ($r.Status -ne 200) { throw "GET /v1/state: $($r.Status)" }
  return $r.Json
}

# Files not yet on the drive (queued, sending or held).
function Get-Unsent {
  $s = Get-State
  return [int]$s.unsent.files + [int]$s.unsent.held
}

function Wait-Drained {
  <# Waits until nothing is left to send; $false on timeout or held files. #>
  param([int] $Seconds)
  $ok = Wait-Until -Seconds $Seconds -Test { $s = Get-State; ([int]$s.unsent.files -eq 0) -and ([int]$s.transfers.files_left -eq 0) }
  if (-not $ok) { return $false }
  return ([int](Get-State).unsent.held -eq 0)
}

# ---- the fake drive's fault controls --------------------------------------------

function Invoke-Faults {
  param([string] $Method, [string] $Path)
  return Invoke-RestMethod -Method $Method -Uri "$Faults$Path" -TimeoutSec 30 -UseBasicParsing
}

function Get-Counts {
  return (Invoke-Faults GET '/stats').counts
}

function Get-Count {
  param($Counts, [string] $Name)
  $p = $Counts.PSObject.Properties[$Name]
  if ($p) { return [int]$p.Value }
  return 0
}

function Get-DriveTree {
  <# The drive's files below a folder: relative path -> entry (kind, id, size, versions, sha256). #>
  param([string] $Folder)
  $map = @{}
  $list = Invoke-Faults GET ('/tree?path=' + [Uri]::EscapeDataString($Folder))
  # (Windows PowerShell hands a JSON array over as one object: foreach unrolls it.)
  foreach ($e in $list) { if ($e -and $e.path) { $map[$e.path] = $e } }
  return $map
}

function Get-LocalTree {
  <# The same for a folder on this PC (or on the mounted drive). #>
  param([string] $Folder)
  $map = @{}
  $base = (Resolve-Path -LiteralPath $Folder).ProviderPath.TrimEnd('\')
  foreach ($i in Get-ChildItem -LiteralPath $Folder -Recurse -Force) {
    $rel = $i.FullName.Substring($base.Length + 1).Replace('\', '/')
    if ($i.PSIsContainer) {
      $map[$rel] = New-Object psobject -Property @{ kind = 'folder'; size = 0; sha256 = '' }
    } else {
      $h = (Get-FileHash -LiteralPath $i.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
      $map[$rel] = New-Object psobject -Property @{ kind = 'file'; size = $i.Length; sha256 = $h }
    }
  }
  return $map
}

function Compare-Trees {
  <# Differences between two trees as text lines (none when identical). #>
  param([hashtable] $Want, [hashtable] $Got)
  $diff = New-Object System.Collections.ArrayList
  foreach ($k in $Want.Keys) {
    if (-not $Got.ContainsKey($k)) { [void]$diff.Add("missing on the drive: $k"); continue }
    $w = $Want[$k]; $g = $Got[$k]
    if ($w.kind -ne $g.kind) { [void]$diff.Add("$k is a $($w.kind) here, a $($g.kind) on the drive"); continue }
    if ($w.kind -eq 'file' -and (([int64]$w.size -ne [int64]$g.size) -or ($w.sha256 -ne $g.sha256))) {
      [void]$diff.Add("$k differs ($($w.size) bytes here, $($g.size) on the drive)")
    }
  }
  foreach ($k in $Got.Keys) { if (-not $Want.ContainsKey($k)) { [void]$diff.Add("only on the drive: $k") } }
  return $diff
}

# ---- test data --------------------------------------------------------------------

function New-TestFile {
  <# Writes $Size bytes of deterministic synthetic data (seeded) to $Path. #>
  param([string] $Path, [int64] $Size, [int] $Seed)
  $rng = New-Object System.Random $Seed
  $block = New-Object byte[] ([Math]::Min([int64]1MB, [Math]::Max([int64]1, $Size)))
  $rng.NextBytes($block)
  $f = [System.IO.File]::Open($Path, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
  try {
    $left = $Size
    $n = 0
    while ($left -gt 0) {
      # every block differs: its number in the first eight bytes
      [BitConverter]::GetBytes([int64]$n).CopyTo($block, 0)
      $c = [int][Math]::Min([int64]$block.Length, $left)
      $f.Write($block, 0, $c)
      $left -= $c
      $n++
    }
  } finally { $f.Close() }
}

function Join-Rel {
  <# Joins relative paths; an empty parent is the top. #>
  param([string] $Parent, [string] $Name)
  if ($Parent) { return "$Parent\$Name" }
  return $Name
}

function Measure-Seconds {
  param([scriptblock] $Block)
  $sw = [System.Diagnostics.Stopwatch]::StartNew()
  $err = $null
  try { & $Block | Out-Null } catch { $err = $_ }
  $sw.Stop()
  return New-Object psobject -Property @{ Seconds = $sw.Elapsed.TotalSeconds; Error = $err }
}

# ---- set-up ------------------------------------------------------------------------

$Od = Join-Path (Resolve-Path $Bin) 'olsyn-drive.exe'
$Fd = Join-Path (Resolve-Path $Bin) 'fakedrive-server.exe'
if (-not (Test-Path $Od)) { throw "missing $Od" }

$Work = Join-Path $env:TEMP 'olsyn-robustness-work'
if (Test-Path $Work) { Remove-Item -Recurse -Force $Work }
New-Item -ItemType Directory -Path $Work | Out-Null
$TestHome = Join-Path $Work 'home'

$fdProc = $null
$startedFakeDrive = $false
if ($Server -eq '') {
  if (-not (Test-Path $Fd)) { throw "missing $Fd" }
  $Server = 'http://127.0.0.1:8787'
  if ($Faults -eq '') { $Faults = 'http://127.0.0.1:8788' }
  $fdData = Join-Path $Work 'fd-data'
  New-Item -ItemType Directory -Path $fdData | Out-Null
  # Windows PowerShell 5.1 does not quote -ArgumentList items: quote the path.
  $fdProc = Start-Process -FilePath $Fd -ArgumentList "--data `"$fdData`" --auto-approve" -WindowStyle Minimized -PassThru
  $startedFakeDrive = $true
} elseif ($Faults -eq '') {
  $u = [Uri]$Server
  $Faults = "$($u.Scheme)://$($u.Host):$($u.Port + 1)"
}
$Faults = $Faults.TrimEnd('/')

$env:OLSYN_DRIVE_HOME = $TestHome
$env:OLSYN_DRIVE_SERVER = $Server
Remove-Item Env:\OLSYN_DRIVE_TOKEN_STORE -ErrorAction SilentlyContinue

$script:agent = $null
function Start-Agent {
  $script:agent = Start-Process -FilePath $Od -ArgumentList 'agent' -WindowStyle Hidden -PassThru
  return (Wait-Until -Seconds 30 -Test { $s = Get-State; $s.ready })
}

function Wait-Mounted {
  param([int] $Seconds = 60)
  return (Wait-Until -Seconds $Seconds -Test { (Get-State).mount.state -eq 'mounted' })
}

$Root = "Robustness $(Get-Date -Format 'yyyyMMdd-HHmmss')"
$Drive = $null

try {
  $ok = Wait-Until -Seconds 20 -Test { (Invoke-WebRequest -UseBasicParsing -Uri "$Faults/stats" -TimeoutSec 2).StatusCode -eq 200 }
  Step 'fake drive answers' $ok $Faults
  if (-not $ok) { throw 'the fake drive did not answer' }

  Step 'agent starts' (Start-Agent)
  $r = Invoke-Agent POST '/v1/signin' '{"open_browser":false}'
  [void](Invoke-Faults POST '/approve')
  $ok = Wait-Until -Seconds 30 -Test { (Get-State).account -ne $null }
  Step 'signed in to the fake drive' $ok "signin status $($r.Status)"
  if (-not $ok) { throw 'sign-in failed' }

  $s = Get-State
  if (@('missing', 'outdated') -contains $s.driver.state) {
    Note 'WinFsp is missing: approve the Windows prompt that opens now'
    [void](Invoke-Agent POST '/v1/driver/install' '{}')
    $ok = Wait-Until -Seconds 300 -Test { @('ok', 'restart_needed') -contains (Get-State).driver.state }
    $d = (Get-State).driver.state
    Step 'WinFsp installed' ($ok -and $d -eq 'ok') "driver state $d"
    if ($d -ne 'ok') { throw 'WinFsp is not usable yet (sign out of Windows and in if it asked for that)' }
  }

  $set = (Invoke-Agent GET '/v1/settings').Json
  $want = $Letter.ToUpperInvariant()
  if (-not (@($set.choices.drive_letters) -contains $want)) { $want = $set.choices.suggested_letter }
  $r = Invoke-Agent POST '/v1/mount' ('{"letter":"' + $want + '"}')
  $ok = Wait-Mounted
  $Drive = (Get-State).mount.path
  Step "drive mounted at $want" $ok "mount status $($r.Status), path $Drive"
  if (-not $ok) { throw 'the drive did not mount' }
  $DriveRoot = Join-Path $Drive $Root
  New-Item -ItemType Directory -Path $DriveRoot | Out-Null

  # ---- a: kill mid-upload, resume from finished parts ----
  try {
    $src = Join-Path $Work 'a-src'
    New-Item -ItemType Directory -Path $src | Out-Null
    for ($i = 1; $i -le 500; $i++) { New-TestFile (Join-Path $src ('small-{0:D3}.bin' -f $i)) (1KB + ($i * 97) % 64KB) $i }
    $bigSize = [int64]1GB
    New-TestFile (Join-Path $src 'big.bin') $bigSize 7
    $parts = [int][Math]::Ceiling($bigSize / 16MB) # the fake drive's part size
    # Over loopback the whole file goes up in a few seconds, before the kill
    # can land. Throttle the agent's uploads (16 MiB/s: about a minute for
    # the file) so the kill finds parts still to send.
    $throttle = Invoke-Agent PATCH '/v1/settings' '{"bandwidth":{"up_bps":16777216}}'
    Step 'a: uploads throttled to 16 MiB/s for the kill' ($throttle.Status -eq 200) "status $($throttle.Status)"
    $counts = Get-Counts
    $base = Get-Count $counts 'part.stored.multipart'
    $baseAgain = Get-Count $counts 'part.stored.again'
    $dst = Join-Path $DriveRoot 'a'
    $t = Measure-Seconds { Copy-Item -LiteralPath $src -Destination $dst -Recurse }
    Step 'a: 500 files and 1 GiB copied onto the drive' ($t.Error -eq $null) ("{0:N1} s {1}" -f $t.Seconds, $t.Error)
    $quarter = [int]($parts / 4)
    $ok = Wait-Until -Seconds 900 -Test { (Get-Count (Get-Counts) 'part.stored.multipart') - $base -ge $quarter }
    taskkill /f /pid $script:agent.Id | Out-Null
    $done = (Get-Count (Get-Counts) 'part.stored.multipart') - $base
    Step 'a: agent killed while the large file was uploading' ($ok -and $done -lt $parts) "$done of $parts parts stored at the kill"
    Start-Sleep -Seconds 2
    Step 'a: agent restarts' (Start-Agent)
    Step 'a: drive mounted again' (Wait-Mounted)
    $unthrottle = Invoke-Agent PATCH '/v1/settings' '{"bandwidth":{"up_bps":0}}'
    Step 'a: upload limit lifted after the restart' ($unthrottle.Status -eq 200) "status $($unthrottle.Status)"
    Step 'a: every file sent after the restart' (Wait-Drained -Seconds 1800)
    $counts = Get-Counts
    $total = (Get-Count $counts 'part.stored.multipart') - $base
    $again = (Get-Count $counts 'part.stored.again') - $baseAgain
    # Resuming sends each part not stored before the kill once (a part cut
    # off mid-way by the kill was never stored); a part stored before the
    # kill is never sent again.
    Step 'a: the large file resumed from its finished parts' (($done -lt $parts) -and ($total -eq $parts) -and ($again -eq 0)) "$total parts stored for $parts ($done before the kill, $again stored twice)"
    $diff = Compare-Trees (Get-LocalTree $src) (Get-DriveTree "/$Root/a")
    Step 'a: the drive holds the same 501 files' ($diff.Count -eq 0) (($diff | Select-Object -First 5) -join '; ')
    Remove-Item -Recurse -Force $src
  } catch { Step 'a: kill mid-upload' $false "$_" }

  # ---- b: drive offline for 60 s during a copy ----
  try {
    $src = Join-Path $Work 'b-src'
    New-Item -ItemType Directory -Path $src | Out-Null
    for ($i = 1; $i -le 200; $i++) { New-TestFile (Join-Path $src ('copy-{0:D3}.bin' -f $i)) 256KB (1000 + $i) }
    $dst = Join-Path $DriveRoot 'b'
    $job = Start-Job -ScriptBlock { param($s, $d) Copy-Item -LiteralPath $s -Destination $d -Recurse } -ArgumentList $src, $dst
    [void](Wait-Until -Seconds 30 -Test { @(Get-ChildItem -LiteralPath $dst -ErrorAction Stop).Count -ge 5 })
    [void](Invoke-Faults POST '/offline?on=1')
    $offAt = Get-Date
    $ok = Wait-Until -Seconds 20 -Test { (Get-State).connection -eq 'offline' }
    Step 'b: agent reports offline' $ok
    # Data that is not cached: a file of the seeded large folder never opened here.
    $uncached = Join-Path $Drive 'Large folder\item-04321.txt'
    $t = Measure-Seconds { [System.IO.File]::ReadAllBytes($uncached) }
    $how = 'answered'
    if ($t.Error) { $how = 'failed' }
    Step 'b: reading data that is not cached fails or answers within 2 s' ($t.Seconds -le 2.5) ("{0} after {1:N2} s" -f $how, $t.Seconds)
    $written = Join-Path $DriveRoot 'b-offline'
    $t = Measure-Seconds {
      New-Item -ItemType Directory -Path $written | Out-Null
      for ($i = 1; $i -le 20; $i++) { New-TestFile (Join-Path $written ('offline-{0:D2}.bin' -f $i)) 64KB (2000 + $i) }
    }
    Step 'b: writes go on into the cache while offline' ($t.Error -eq $null) ("20 files in {0:N2} s {1}" -f $t.Seconds, $t.Error)
    $left = 60 - ((Get-Date) - $offAt).TotalSeconds
    if ($left -gt 0) { Start-Sleep -Seconds ([int][Math]::Ceiling($left)) }
    [void](Invoke-Faults POST '/offline?on=0')
    $jobOk = Wait-Job $job -Timeout 600
    $jobErr = $null
    try { Receive-Job $job -ErrorAction Stop | Out-Null } catch { $jobErr = $_ }
    Remove-Job $job -Force
    Step 'b: the copy finished' (($jobOk -ne $null) -and ($jobErr -eq $null)) "$jobErr"
    Step 'b: the backlog drains once the drive is back' (Wait-Drained -Seconds 900)
    $diff = Compare-Trees (Get-LocalTree $src) (Get-DriveTree "/$Root/b")
    $diff2 = Compare-Trees (Get-LocalTree $written) (Get-DriveTree "/$Root/b-offline")
    Step 'b: the drive holds every file copied and written' (($diff.Count + $diff2.Count) -eq 0) ((@($diff) + @($diff2) | Select-Object -First 5) -join '; ')
    Remove-Item -Recurse -Force $src
  } catch {
    try { [void](Invoke-Faults POST '/offline?on=0') } catch { }
    Step 'b: offline during a copy' $false "$_"
  }

  # ---- c: sign-out with unsent files is refused without confirmation ----
  try {
    [void](Invoke-Faults POST '/offline?on=1')
    $dir = Join-Path $DriveRoot 'c'
    New-Item -ItemType Directory -Path $dir | Out-Null
    for ($i = 1; $i -le 3; $i++) { New-TestFile (Join-Path $dir "unsent-$i.bin") 128KB (3000 + $i) }
    $before = Get-LocalTree $dir
    $ok = Wait-Until -Seconds 20 -Test { (Get-Unsent) -ge 3 }
    $r = Invoke-Agent POST '/v1/signout' '{"clear_cache":true,"discard_unsent_confirmed":false}'
    $code = ''
    if ($r.Json -and $r.Json.error) { $code = [string]$r.Json.error }
    Step 'c: sign-out with unsent files is refused without confirmation' ($ok -and $r.Status -eq 409 -and $code -eq 'unsent_data') "status $($r.Status) $code"
    $s = Get-State
    $diff = Compare-Trees $before (Get-LocalTree $dir)
    Step 'c: still signed in, drive mounted, files intact' (($s.account -ne $null) -and ($s.mount.state -eq 'mounted') -and ($diff.Count -eq 0)) (($diff | Select-Object -First 3) -join '; ')
    [void](Invoke-Faults POST '/offline?on=0')
    Step 'c: the files are sent once the drive is back' (Wait-Drained -Seconds 300)
    $diff = Compare-Trees $before (Get-DriveTree "/$Root/c")
    Step 'c: the drive holds them unchanged' ($diff.Count -eq 0) (($diff | Select-Object -First 3) -join '; ')
  } catch {
    try { [void](Invoke-Faults POST '/offline?on=0') } catch { }
    Step 'c: sign-out refused' $false "$_"
  }

  # ---- d: quit and restart with unsent files ----
  try {
    [void](Invoke-Faults POST '/offline?on=1')
    $dir = Join-Path $DriveRoot 'd'
    New-Item -ItemType Directory -Path $dir | Out-Null
    for ($i = 1; $i -le 5; $i++) { New-TestFile (Join-Path $dir "queued-$i.bin") 200KB (4000 + $i) }
    $before = Get-LocalTree $dir
    $ok = Wait-Until -Seconds 20 -Test { (Get-Unsent) -ge 5 }
    $r = Invoke-Agent POST '/v1/quit' '{"unmount":true}'
    $exited = $script:agent.WaitForExit(60000)
    Step 'd: agent quits with 5 unsent files' ($ok -and ($r.Status -eq 202) -and $exited) "status $($r.Status)"
    [void](Invoke-Faults POST '/offline?on=0')
    Step 'd: agent restarts' (Start-Agent)
    Step 'd: drive mounted again' (Wait-Mounted)
    Step 'd: the queued files finish' (Wait-Drained -Seconds 300)
    $diff = Compare-Trees $before (Get-DriveTree "/$Root/d")
    Step 'd: the drive holds them unchanged' ($diff.Count -eq 0) (($diff | Select-Object -First 3) -join '; ')
  } catch {
    try { [void](Invoke-Faults POST '/offline?on=0') } catch { }
    Step 'd: quit and restart' $false "$_"
  }

  # ---- e: a storm of 1,000 operations ----
  try {
    $top = Join-Path $DriveRoot 'e'
    New-Item -ItemType Directory -Path $top | Out-Null
    $rng = New-Object System.Random 42
    $folders = New-Object System.Collections.ArrayList
    $files = New-Object System.Collections.ArrayList
    [void]$folders.Add('')
    $counts = @{ mkdir = 0; create = 0; overwrite = 0; rename = 0; delete = 0 }
    $n = 0
    for ($op = 1; $op -le 1000; $op++) {
      $roll = $rng.Next(100)
      $n++
      if ($roll -lt 12) {
        $rel = Join-Rel $folders[$rng.Next($folders.Count)] "dir-$n"
        New-Item -ItemType Directory -Path (Join-Path $top $rel) | Out-Null
        [void]$folders.Add($rel); $counts.mkdir++
      } elseif ($roll -lt 45 -or $files.Count -lt 5) {
        $rel = Join-Rel $folders[$rng.Next($folders.Count)] "file-$n.dat"
        New-TestFile (Join-Path $top $rel) ($rng.Next(32768)) $n
        [void]$files.Add($rel); $counts.create++
      } elseif ($roll -lt 65) {
        $rel = $files[$rng.Next($files.Count)]
        New-TestFile (Join-Path $top $rel) ($rng.Next(32768)) (10000 + $n)
        $counts.overwrite++
      } elseif ($roll -lt 82) {
        $i = $rng.Next($files.Count)
        $new = Join-Rel (Split-Path -Parent $files[$i]) "renamed-$n.dat"
        Rename-Item -LiteralPath (Join-Path $top $files[$i]) -NewName (Split-Path -Leaf $new)
        $files[$i] = $new; $counts.rename++
      } else {
        $i = $rng.Next($files.Count)
        Remove-Item -LiteralPath (Join-Path $top $files[$i])
        $files.RemoveAt($i); $counts.delete++
      }
    }
    # a few folder renames and a folder delete, with what is inside them
    for ($k = 0; $k -lt 5 -and $folders.Count -gt 2; $k++) {
      $i = 1 + $rng.Next($folders.Count - 1)
      $old = $folders[$i]
      $new = Join-Rel (Split-Path -Parent $old) "moved-$k"
      Rename-Item -LiteralPath (Join-Path $top $old) -NewName (Split-Path -Leaf $new)
      for ($j = 0; $j -lt $folders.Count; $j++) { if ($folders[$j] -eq $old -or $folders[$j].StartsWith("$old\")) { $folders[$j] = $new + $folders[$j].Substring($old.Length) } }
      for ($j = 0; $j -lt $files.Count; $j++) { if ($files[$j].StartsWith("$old\")) { $files[$j] = $new + $files[$j].Substring($old.Length) } }
    }
    Note ("e: {0} new folders, {1} new files, {2} overwrites, {3} renames, {4} deletes, 5 folder renames" -f $counts.mkdir, $counts.create, $counts.overwrite, $counts.rename, $counts.delete)
    Step 'e: the storm settles' (Wait-Drained -Seconds 900)
    $diff = Compare-Trees (Get-LocalTree $top) (Get-DriveTree "/$Root/e")
    Step 'e: the drive and the mounted copy are identical' ($diff.Count -eq 0) ("$($diff.Count) differences: " + (($diff | Select-Object -First 5) -join '; '))
  } catch { Step 'e: storm' $false "$_" }

  # ---- f: large reads ----
  try {
    $big = @((Get-DriveTree '/Large file').Values | Where-Object { $_.kind -eq 'file' } | Sort-Object size -Descending)[0]
    $path = Join-Path $Drive ('Large file\' + $big.path)
    $tail = 1MB
    $buf = New-Object byte[] $tail
    $t = Measure-Seconds {
      $f = [System.IO.File]::Open($path, 'Open', 'Read', 'ReadWrite')
      try {
        [void]$f.Seek([int64]$big.size - $tail, 'Begin')
        $got = 0
        while ($got -lt $tail) { $n = $f.Read($buf, $got, $tail - $got); if ($n -le 0) { break }; $got += $n }
        if ($got -ne $tail) { throw "read $got bytes" }
      } finally { $f.Close() }
    }
    Step 'f: the last 1 MiB of the large generated file reads' ($t.Error -eq $null) ("{0:N0} MiB file, {1:N2} s {2}" -f ($big.size / 1MB), $t.Seconds, $t.Error)
    $chunk = New-Object byte[] (4MB)
    $want = [int64]1GB
    $t = Measure-Seconds {
      $f = [System.IO.File]::Open($path, 'Open', 'Read', 'ReadWrite')
      try {
        $read = [int64]0
        while ($read -lt $want) { $n = $f.Read($chunk, 0, $chunk.Length); if ($n -le 0) { break }; $read += $n }
        if ($read -lt $want) { throw "read only $read bytes" }
      } finally { $f.Close() }
    }
    Step 'f: 1 GiB read sequentially' ($t.Error -eq $null) "$($t.Error)"
    if ($t.Seconds -gt 0) { Note ("f: 1 GiB sequential read in {0:N1} s = {1:N0} MiB/s" -f $t.Seconds, (1024 / $t.Seconds)) }
  } catch { Step 'f: large reads' $false "$_" }

  # ---- g: Word-style save while another process keeps the file open ----
  try {
    $dir = Join-Path $DriveRoot 'g'
    New-Item -ItemType Directory -Path $dir | Out-Null
    $doc = Join-Path $dir 'Report.docx'
    New-TestFile $doc 96KB 5001
    [void](Wait-Drained -Seconds 120)
    $first = (Get-DriveTree "/$Root/g")['Report.docx']
    # Another program (a preview, a backup tool) holds the document open, sharing read, write and delete.
    $holdCmd = "`$f = [System.IO.File]::Open('$doc', 'Open', 'Read', 'ReadWrite, Delete'); Start-Sleep -Seconds 8; `$f.Close()"
    $enc = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($holdCmd))
    $holder = Start-Process -FilePath powershell -ArgumentList '-NoProfile', '-EncodedCommand', $enc -WindowStyle Hidden -PassThru
    Start-Sleep -Seconds 2
    $tmp = Join-Path $dir '~WRL0001.tmp'
    $backup = Join-Path $dir '~WRL0002.tmp'
    New-TestFile $tmp 100KB 5002
    $newHash = (Get-FileHash -LiteralPath $tmp -Algorithm SHA256).Hash.ToLowerInvariant()
    [System.IO.File]::Move($doc, $backup)
    [System.IO.File]::Move($tmp, $doc)
    try { Remove-Item -LiteralPath $backup -Force } catch { [void]$holder.WaitForExit(15000); Remove-Item -LiteralPath $backup -Force }
    [void]$holder.WaitForExit(15000)
    Step 'g: saved the Word way while another process held the file' $true
    Step 'g: the save reaches the drive' (Wait-Drained -Seconds 120)
    $tree = Get-DriveTree "/$Root/g"
    $after = $tree['Report.docx']
    $names = (@($tree.Keys) | Sort-Object) -join ', '
    $okVersion = ($first -ne $null) -and ($after -ne $null) -and ($after.id -eq $first.id) -and ([int]$after.versions -eq [int]$first.versions + 1) -and ($after.sha256 -eq $newHash)
    Step 'g: the drive has a new version of the same file, not a delete and a new file' $okVersion ("before id {0} v{1}, after id {2} v{3}" -f $first.id, $first.versions, $after.id, $after.versions)
    Step 'g: no temp files on the drive' ($tree.Count -eq 1) $names
  } catch { Step 'g: Word-style save' $false "$_" }
}
catch {
  Step 'robustness run' $false "$_"
}
finally {
  if (-not $KeepRunning) {
    try { [void](Invoke-Faults POST '/offline?on=0') } catch { }
    try { [void](Invoke-Agent POST '/v1/quit' '{"unmount":true}' -TimeoutSeconds 10) } catch { }
    if ($script:agent -and -not $script:agent.HasExited) { [void]$script:agent.WaitForExit(30000) }
    if ($script:agent -and -not $script:agent.HasExited) { Stop-Process -Id $script:agent.Id -Force -ErrorAction SilentlyContinue }
    if ($startedFakeDrive -and $fdProc -and -not $fdProc.HasExited) { Stop-Process -Id $fdProc.Id -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Seconds 1
    Remove-Item -Recurse -Force $Work -ErrorAction SilentlyContinue
  } else {
    Write-Output "left running: the agent and the fake drive (home $TestHome)"
  }
}

if ($script:Fails -eq 0) { $summary = 'All checks passed.' } else { $summary = "$($script:Fails) check(s) failed." }
Write-Result $summary
Write-Output $summary
Write-Output "results: $Result"
exit $script:Fails
