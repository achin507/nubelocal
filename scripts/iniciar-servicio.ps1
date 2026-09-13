param(
  [switch]$SkipBuild,     # no recompilar el frontend
  [switch]$Foreground,    # correr en esta ventana y ver el log en vivo
  [switch]$Force          # matar lo que ocupe el puerto y arrancar limpio
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "comun.ps1")

$root       = Get-ProjectRoot
$runtimeDir = Join-Path $root "runtime"
$goCacheDir = Join-Path $runtimeDir "go-cache"
$pidFile    = Join-Path $runtimeDir "nube-lan.pid"
$logFile    = Join-Path $runtimeDir "nube-lan.log"
$errFile    = Join-Path $runtimeDir "nube-lan.err.log"
$exeFile    = Join-Path $runtimeDir "nube-lan-server.exe"

function Get-GoExe {
  $candidates = @(
    "go",
    "C:\Program Files\Go\bin\go.exe",
    "C:\Program Files (x86)\Go\bin\go.exe",
    (Join-Path $root ".tools\go\bin\go.exe")
  )
  foreach ($candidate in $candidates) {
    $cmd = Get-Command $candidate -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    if (Test-Path -LiteralPath $candidate) { return $candidate }
  }
  throw "Go no esta instalado o no esta en PATH."
}

Set-Location $root
New-Item -ItemType Directory -Force -Path $runtimeDir | Out-Null
New-Item -ItemType Directory -Force -Path $goCacheDir | Out-Null
$env:GOCACHE = $goCacheDir

if (!(Test-Path -LiteralPath (Join-Path $root ".env"))) {
  Copy-Item (Join-Path $root ".env.example") (Join-Path $root ".env")
  Write-Host "Cree .env desde .env.example. Cambia APP_PASSWORD cuando quieras tu propia clave."
}

$port = [int](Get-EnvValue $root "PORT" "8443")

# ---------------------------------------------------------------------------
# 1. Puerto ocupado: decir la verdad. Antes se imprimia "ya esta activo" y se
#    salia con exito sin arrancar nada, aunque el proceso fuera basura vieja.
# ---------------------------------------------------------------------------
$owner = Get-PortOwnerProcess $port
if ($owner) {
  $esMio = ($owner.Path -and $owner.Path -eq $exeFile)

  if ($esMio -and -not $Force) {
    if (Test-ServiceResponds $port) {
      Write-Host "Nube LAN Pro ya estaba corriendo y responde. PID: $($owner.ProcessId)"
      Set-Content -LiteralPath $pidFile -Value $owner.ProcessId -NoNewline
      Show-AccessUrls $port
      Show-FirewallHint $port
      exit 0
    }
    Write-Host "El proceso $($owner.ProcessId) ocupa el puerto $port pero NO responde. Lo reinicio."
    $Force = $true
  }

  if (-not $esMio -and -not $Force) {
    Write-Host ""
    Write-Host "ERROR: el puerto $port ya esta ocupado por otro proceso." -ForegroundColor Red
    Write-Host "  PID    : $($owner.ProcessId)"
    Write-Host "  Nombre : $($owner.Name)"
    if ($owner.Path) { Write-Host "  Ruta   : $($owner.Path)" }
    Write-Host ""
    Write-Host "Si es una copia vieja de este servidor, arranca asi para reemplazarla:"
    Write-Host "  .\scripts\iniciar-servicio.ps1 -Force"
    Write-Host ""
    exit 1
  }

  Write-Host "Cierro el proceso $($owner.ProcessId) que ocupaba el puerto $port..."
  Stop-Process -Id $owner.ProcessId -Force -ErrorAction SilentlyContinue
  $limite = (Get-Date).AddSeconds(10)
  while ((Get-PortOwnerProcess $port) -and (Get-Date) -lt $limite) { Start-Sleep -Milliseconds 300 }
  if (Get-PortOwnerProcess $port) {
    Write-Host "No pude liberar el puerto $port." -ForegroundColor Red
    exit 1
  }
}

if (Test-Path -LiteralPath $pidFile) { Remove-Item -LiteralPath $pidFile -Force }

# ---------------------------------------------------------------------------
# 2. Frontend
# ---------------------------------------------------------------------------
if (!$SkipBuild) {
  Push-Location (Join-Path $root "web")
  try {
    if (!(Test-Path "node_modules")) { npm install }
    npm run build
    if ($LASTEXITCODE -ne 0) { throw "Fallo el build del frontend (npm run build)." }
  } finally {
    Pop-Location
  }
}

# ---------------------------------------------------------------------------
# 3. Compilar un binario de verdad.
#    Antes se usaba "go run", que deja tres procesos encadenados
#    (powershell -> go.exe -> server.exe en %TEMP%). Matar el primero no mata
#    a los otros dos, y por eso el puerto quedaba tomado para siempre.
# ---------------------------------------------------------------------------
$goExe = Get-GoExe

# Windows no libera el .exe en el mismo instante en que muere el proceso. Si se
# compila enseguida despues de detener, el build muere con "Access is denied" y
# el arranque se cae entero. Se espera a que el archivo se pueda escribir.
if (Test-Path -LiteralPath $exeFile) {
  $limite = (Get-Date).AddSeconds(15)
  while ((Get-Date) -lt $limite) {
    try {
      $fs = [System.IO.File]::Open($exeFile, 'Open', 'Write', 'None')
      $fs.Close()
      break
    } catch {
      Start-Sleep -Milliseconds 300
    }
  }
}

Write-Host "Compilando el servidor..."
& $goExe build -o $exeFile ./cmd/server
if ($LASTEXITCODE -ne 0) {
  Write-Host "Fallo la compilacion. No se arranco nada." -ForegroundColor Red
  exit 1
}

if ($Foreground) {
  Write-Host "Iniciando en primer plano. Ctrl+C para detener."
  Show-FirewallHint $port
  & $exeFile
  exit $LASTEXITCODE
}

# ---------------------------------------------------------------------------
# 4. Arrancar el binario directamente: un solo proceso, un solo PID.
# ---------------------------------------------------------------------------
Remove-Item -LiteralPath $logFile, $errFile -ErrorAction SilentlyContinue

$process = Start-Process -FilePath $exeFile `
                         -WorkingDirectory $root `
                         -WindowStyle Hidden `
                         -RedirectStandardOutput $logFile `
                         -RedirectStandardError $errFile `
                         -PassThru

Set-Content -LiteralPath $pidFile -Value $process.Id -NoNewline

# ---------------------------------------------------------------------------
# 5. No declarar exito hasta que la pagina conteste de verdad.
# ---------------------------------------------------------------------------
Write-Host "Esperando a que responda..."
$listo   = $false
$limite  = (Get-Date).AddSeconds(25)
while ((Get-Date) -lt $limite) {
  if (-not (Get-Process -Id $process.Id -ErrorAction SilentlyContinue)) { break }
  if (Test-ServiceResponds $port 3) { $listo = $true; break }
  Start-Sleep -Milliseconds 500
}

if (-not $listo) {
  Write-Host ""
  Write-Host "NO ARRANCO." -ForegroundColor Red
  Remove-Item -LiteralPath $pidFile -ErrorAction SilentlyContinue
  Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
  foreach ($f in @($errFile, $logFile)) {
    if ((Test-Path -LiteralPath $f) -and (Get-Item -LiteralPath $f).Length -gt 0) {
      Write-Host "--- $(Split-Path -Leaf $f) ---"
      Get-Content -LiteralPath $f -Tail 25
    }
  }
  exit 1
}

Write-Host "Nube LAN Pro iniciado y respondiendo. PID: $($process.Id)"
Show-AccessUrls $port
Show-FirewallHint $port
Write-Host "  Log: $logFile"
