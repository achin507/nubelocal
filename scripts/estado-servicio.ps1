$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "comun.ps1")

$root       = Get-ProjectRoot
$runtimeDir = Join-Path $root "runtime"
$pidFile    = Join-Path $runtimeDir "nube-lan.pid"
$exeFile    = Join-Path $runtimeDir "nube-lan-server.exe"
$port       = [int](Get-EnvValue $root "PORT" "8443")

Write-Host ""
Write-Host "=== Nube LAN Pro - estado ==="
Write-Host ""

$owner = Get-PortOwnerProcess $port
if (-not $owner) {
  Write-Host "Servidor  : DETENIDO (nadie escucha en el puerto $port)"
} else {
  $mio = "otro programa"
  if ($owner.Path -and $owner.Path -eq $exeFile) { $mio = "este proyecto" }
  Write-Host "Puerto $port : ocupado por PID $($owner.ProcessId) ($($owner.Name)) - $mio"
  if ($owner.Path) { Write-Host "Binario   : $($owner.Path)" }

  if (Test-ServiceResponds $port) {
    Write-Host "Respuesta : OK, la pagina contesta"
  } else {
    Write-Host "Respuesta : NO CONTESTA aunque el puerto este tomado" -ForegroundColor Red
  }
}

if (Test-Path -LiteralPath $pidFile) {
  $texto = (Get-Content -Raw -LiteralPath $pidFile).Trim()
  $vivo = Get-Process -Id $texto -ErrorAction SilentlyContinue
  if ($vivo) {
    Write-Host "nube-lan.pid : $texto (vivo)"
  } else {
    Write-Host "nube-lan.pid : $texto (MUERTO, archivo viejo)"
  }
} else {
  Write-Host "nube-lan.pid : no existe"
}

Write-Host ""
Write-Host "Direccion de hoy:"
Show-AccessUrls $port
Show-FirewallHint $port
