# Funciones compartidas por iniciar / detener / estado.
# No se ejecuta sola: se carga con dot-sourcing.

function Get-ProjectRoot {
  return (Split-Path -Parent $PSScriptRoot)
}

function Get-EnvValue([string]$root, [string]$name, [string]$fallback) {
  $envPath = Join-Path $root ".env"
  if (Test-Path -LiteralPath $envPath) {
    $line = Select-String -LiteralPath $envPath -Pattern "^$name=" -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($line) { return $line.Line.Split("=", 2)[1].Trim('"').Trim("'") }
  }
  return $fallback
}

# La IP que ven los demas dispositivos: la de la interfaz con ruta a Internet.
# Se calcula en cada arranque, asi que si el router cambia la direccion el
# enlace que se imprime cambia con ella.
function Get-LanIPv4 {
  $route = Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue |
    Sort-Object -Property RouteMetric, ifMetric | Select-Object -First 1
  if ($route) {
    $addr = Get-NetIPAddress -InterfaceIndex $route.InterfaceIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue |
      Where-Object { $_.IPAddress -notlike '127.*' } | Select-Object -First 1
    if ($addr) { return $addr.IPAddress }
  }

  $fallback = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
    Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' } |
    Select-Object -First 1
  if ($fallback) { return $fallback.IPAddress }

  return $null
}

# Quien escucha en el puerto. Devuelve el proceso, no solo el PID, porque hay
# que distinguir "es mi servidor" de "es otro programa que me robo el puerto".
function Get-PortOwnerProcess([int]$port) {
  $owningPid = $null

  $cmd = Get-Command Get-NetTCPConnection -ErrorAction SilentlyContinue
  if ($cmd) {
    $conn = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($conn) { $owningPid = [int]$conn.OwningProcess }
  }

  if (-not $owningPid) {
    $line = netstat -ano -p TCP | Select-String -Pattern ":$port\s" | Select-String -Pattern "LISTENING" | Select-Object -First 1
    if ($line -and $line.Line -match "LISTENING\s+(\d+)\s*$") { $owningPid = [int]$Matches[1] }
  }

  if (-not $owningPid) { return $null }

  $proc = Get-Process -Id $owningPid -ErrorAction SilentlyContinue
  if (-not $proc) { return $null }

  $path = $null
  try { $path = $proc.Path } catch { $path = $null }

  return [pscustomobject]@{
    ProcessId = $owningPid
    Name      = $proc.ProcessName
    Path      = $path
  }
}

# Prueba real: pide la pagina por HTTPS. Que el proceso este vivo no prueba
# que el servidor responda, y ese fue justo el fallo que escondia el script viejo.
function Test-ServiceResponds([int]$port, [int]$timeoutSeconds = 5) {
  $curl = Get-Command curl.exe -ErrorAction SilentlyContinue
  if ($curl) {
    $code = & curl.exe -sk -o NUL -w "%{http_code}" --max-time $timeoutSeconds "https://127.0.0.1:$port/transferencia" 2>$null
    if ($code -match '^\d+$' -and [int]$code -ge 200 -and [int]$code -lt 500) { return $true }
    return $false
  }

  try {
    add-type -TypeDefinition @'
using System.Net;
using System.Security.Cryptography.X509Certificates;
public class AceptaCertLocal {
  public static void Instalar() {
    ServicePointManager.ServerCertificateValidationCallback =
      delegate(object s, X509Certificate c, X509Chain ch, System.Net.Security.SslPolicyErrors e) { return true; };
    ServicePointManager.SecurityProtocol = SecurityProtocolType.Tls12;
  }
}
'@ -ErrorAction SilentlyContinue
    [AceptaCertLocal]::Instalar()
  } catch { }

  try {
    $req = [System.Net.HttpWebRequest]::Create("https://127.0.0.1:$port/transferencia")
    $req.Timeout = $timeoutSeconds * 1000
    $resp = $req.GetResponse()
    $resp.Close()
    return $true
  } catch {
    return $false
  }
}

function Show-AccessUrls([int]$port) {
  Write-Host ""
  Write-Host "  En esta PC : https://localhost:$port/transferencia"
  $ip = Get-LanIPv4
  if ($ip) {
    Write-Host "  Otro equipo: https://$ip`:$port/transferencia"
  } else {
    Write-Host "  Otro equipo: no encontre IP de LAN. Revisa que el Wi-Fi este conectado."
  }
  Write-Host ""
}

# El aviso que faltaba: en una red marcada como Publica, Windows corta las
# conexiones entrantes y el celular nunca llega, aunque el servidor este bien.
function Show-FirewallHint([int]$port) {
  $publica = Get-NetConnectionProfile -ErrorAction SilentlyContinue |
    Where-Object { $_.NetworkCategory -eq 'Public' } | Select-Object -First 1
  if ($publica) {
    Write-Host "AVISO: la red '$($publica.InterfaceAlias)' esta clasificada como Publica."
    Write-Host "       Windows bloquea las conexiones entrantes ahi. Si desde el celular"
    Write-Host "       no carga, abre PowerShell COMO ADMINISTRADOR y ejecuta:"
    Write-Host "         .\scripts\abrir-firewall.ps1"
    Write-Host ""
  }
}
