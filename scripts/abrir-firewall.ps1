# Abre el puerto del servidor en el Firewall de Windows.
# REQUIERE PowerShell abierto COMO ADMINISTRADOR.
#
# Por que hace falta: la red Wi-Fi puede estar clasificada como "Publica", y en
# ese perfil Windows descarta las conexiones entrantes. El servidor arranca bien
# y responde en localhost, pero desde el celular la pagina nunca carga: no da
# error de conexion rechazada, se queda esperando hasta agotar el tiempo.

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "comun.ps1")

$root = Get-ProjectRoot
$port = [int](Get-EnvValue $root "PORT" "8443")
$nombreRegla = "Nube LAN Pro ($port)"

$identidad = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identidad)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Write-Host ""
  Write-Host "Esto necesita permisos de administrador." -ForegroundColor Red
  Write-Host "Cierra esta ventana, busca PowerShell en el menu Inicio,"
  Write-Host "haz clic derecho -> 'Ejecutar como administrador', y vuelve a correr:"
  Write-Host "  cd '$root'"
  Write-Host "  .\scripts\abrir-firewall.ps1"
  Write-Host ""
  exit 1
}

$existente = Get-NetFirewallRule -DisplayName $nombreRegla -ErrorAction SilentlyContinue
if ($existente) {
  Write-Host "La regla '$nombreRegla' ya existe. La dejo habilitada."
  Enable-NetFirewallRule -DisplayName $nombreRegla
} else {
  New-NetFirewallRule -DisplayName $nombreRegla `
                      -Direction Inbound `
                      -Action Allow `
                      -Protocol TCP `
                      -LocalPort $port `
                      -Profile Any `
                      -Description "Permite que otros equipos de la red local abran Nube LAN Pro." | Out-Null
  Write-Host "Regla creada: '$nombreRegla' (TCP $port, entrante, permitir)."
}

Write-Host ""
Write-Host "Listo. Prueba de nuevo desde el celular:"
Show-AccessUrls $port
Write-Host "Para quitarla mas adelante:"
Write-Host "  Remove-NetFirewallRule -DisplayName '$nombreRegla'"
