$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "comun.ps1")

$root       = Get-ProjectRoot
$runtimeDir = Join-Path $root "runtime"
$pidFile    = Join-Path $runtimeDir "nube-lan.pid"
$exeFile    = Join-Path $runtimeDir "nube-lan-server.exe"
$port       = [int](Get-EnvValue $root "PORT" "8443")

$matados = @()

function Stop-Pid([int]$processId, [string]$motivo) {
  $proc = Get-Process -Id $processId -ErrorAction SilentlyContinue
  if (!$proc) { return $false }
  Stop-Process -Id $processId -Force -ErrorAction SilentlyContinue
  Write-Host "  Detenido PID $processId ($($proc.ProcessName)) - $motivo"
  return $true
}

# 1. El PID anotado.
if (Test-Path -LiteralPath $pidFile) {
  $texto = (Get-Content -Raw -LiteralPath $pidFile).Trim()
  if ($texto -match "^\d+$") {
    if (Stop-Pid ([int]$texto) "registrado en nube-lan.pid") { $matados += [int]$texto }
  }
  Remove-Item -LiteralPath $pidFile -Force -ErrorAction SilentlyContinue
}

# 2. Quien tenga el puerto, aunque no sea el del archivo.
#    Esto es lo que faltaba: antes se mataba el powershell envoltorio y los
#    procesos nietos (go.exe y el server.exe temporal) seguian vivos con el
#    puerto tomado, mientras el script informaba "detenido".
$owner = Get-PortOwnerProcess $port
if ($owner -and ($matados -notcontains $owner.ProcessId)) {
  if (Stop-Pid $owner.ProcessId "ocupaba el puerto $port") { $matados += $owner.ProcessId }
}

# 3. Restos de este proyecto: el binario propio y cualquier "go run" suelto.
try {
  $sueltos = Get-CimInstance Win32_Process -ErrorAction Stop | Where-Object {
    ($_.ExecutablePath -and $_.ExecutablePath -eq $exeFile) -or
    ($_.CommandLine -and $_.CommandLine -match [regex]::Escape($root) -and $_.CommandLine -match "go run|cmd/server|cmd\\server")
  }
  foreach ($p in $sueltos) {
    if ($matados -notcontains $p.ProcessId) {
      if (Stop-Pid $p.ProcessId "resto del proyecto") { $matados += $p.ProcessId }
    }
  }
} catch {
  # Algunas configuraciones de Windows no dejan leer la linea de comandos.
}

# 4. Verificar. Decir "detenido" sin comprobarlo fue el error original.
$limite = (Get-Date).AddSeconds(10)
while ((Get-PortOwnerProcess $port) -and (Get-Date) -lt $limite) { Start-Sleep -Milliseconds 300 }

$restante = Get-PortOwnerProcess $port
if ($restante) {
  Write-Host ""
  Write-Host "El puerto $port SIGUE ocupado por el PID $($restante.ProcessId) ($($restante.Name))." -ForegroundColor Red
  if ($restante.Path) { Write-Host "Ruta: $($restante.Path)" }
  exit 1
}

if ($matados.Count -eq 0) {
  Write-Host "No habia ningun servicio activo. El puerto $port esta libre."
} else {
  Write-Host ""
  Write-Host "Nube LAN Pro detenido. Puerto $port libre."
}
