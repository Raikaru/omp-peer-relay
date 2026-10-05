# Windows peer-bus client over tailcat. Works for owners and guests alike.
#
#   setup-windows-client.ps1 -Key
#       Print this machine's tailcat client key (generated on first use).
#       Send it to the relay admin, who runs `tailcat-allow add <name> <key>`.
#
#   setup-windows-client.ps1 -Machine <name> -Address <tailcat-address>
#       Prompts for the relay token (unless ~/.omp/peer-bus.json already has one
#       for this machine), writes the config, and registers a hidden logon task
#       forwarding 127.0.0.1:7480 to the relay.
#
# Requires tailcat.exe in %LOCALAPPDATA%\tailcat and the extension enabled in omp.
param([switch]$Key, [string]$Machine, [string]$Address)
$ErrorActionPreference = "Stop"
$tailcat = Join-Path $env:LOCALAPPDATA "tailcat\tailcat.exe"
if (-not (Test-Path $tailcat)) { throw "tailcat.exe not found at $tailcat (https://github.com/tailscale/tailcat/releases)" }

if ($Key) {
	$keyFile = Join-Path $env:APPDATA "tailcat\keys\client-default.private.json"
	if (-not (Test-Path $keyFile)) { & $tailcat genkey --client --key=client-default | Out-Null }
	(Get-Content $keyFile -Raw | ConvertFrom-Json).Public.ServerPublic
	return
}
if (-not $Machine -or -not $Address) { throw "usage: setup-windows-client.ps1 -Key | -Machine <name> -Address <tailcat-address>" }

$cfgPath = Join-Path $HOME ".omp\peer-bus.json"
$cfg = if (Test-Path $cfgPath) { Get-Content $cfgPath -Raw | ConvertFrom-Json } else { $null }
if (-not $cfg -or $cfg.machine -ne $Machine -or -not $cfg.token) {
	$secure = Read-Host "Relay token for $Machine" -AsSecureString
	$token = [Runtime.InteropServices.Marshal]::PtrToStringBSTR([Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure))
	$cfg = [ordered]@{ url = "ws://127.0.0.1:7480/ws"; token = $token; machine = $Machine }
	New-Item -ItemType Directory -Force (Split-Path $cfgPath) | Out-Null
	[IO.File]::WriteAllText($cfgPath, ($cfg | ConvertTo-Json))  # no BOM: the extension's JSON.parse rejects one
}

$script = Join-Path $PSScriptRoot "omp-relay-tunnel.ps1"
$action = New-ScheduledTaskAction -Execute "powershell.exe" `
	-Argument "-NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$script`" -Address $Address"
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries `
	-DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName "omp-relay-tunnel" -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Stop-ScheduledTask -TaskName "omp-relay-tunnel" -ErrorAction SilentlyContinue
Get-CimInstance Win32_Process -Filter "Name='ssh.exe' OR Name='tailcat.exe'" |
	Where-Object { $_.CommandLine -match '7480' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
Start-ScheduledTask -TaskName "omp-relay-tunnel"
foreach ($i in 1..20) {
	try { (Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 http://127.0.0.1:7480/healthz).Content; return } catch { Start-Sleep 1 }
}
throw "relay not reachable on 127.0.0.1:7480 yet"
