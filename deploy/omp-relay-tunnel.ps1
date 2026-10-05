# Keeps a tailcat forward to omp-peer-relay open (Windows client): 127.0.0.1:7480 -> relay.
# Registered as a hidden logon task by deploy\setup-windows-client.ps1.
param([Parameter(Mandatory)][string]$Address)
$tailcat = Join-Path $env:LOCALAPPDATA "tailcat\tailcat.exe"
while ($true) {
	& $tailcat forward $Address 7480
	Start-Sleep -Seconds 5
}
