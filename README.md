# Nube LAN Pro

Servidor local para transferir archivos en tu red de casa desde el navegador.

## Requisitos

- Go 1.22 o superior
- Node.js 20 o superior

## Uso diario

```powershell
.\scripts\iniciar-servicio.ps1     # arranca
.\scripts\estado-servicio.ps1      # ver si esta vivo y en que direccion
.\scripts\detener-servicio.ps1     # detiene
```

El script de arranque compila, levanta el servidor, **espera a que la pagina
responda de verdad** y recien entonces imprime la direccion. Si algo falla, sale
con error y muestra el log: nunca dice que arranco cuando no arranco.

Opciones:

| Opcion | Para que |
|---|---|
| `-SkipBuild` | no recompilar el frontend (arranque rapido) |
| `-Foreground` | correr en la ventana actual y ver el log en vivo |
| `-Force` | matar lo que este ocupando el puerto y arrancar limpio |

## La direccion cambia sola

No hay ninguna IP escrita a mano. En cada arranque el servidor detecta la IPv4
que le dio el router y la imprime. Si el DHCP te cambia de `192.168.0.9` a
`192.168.0.8`, el enlace que aparece cambia con ella, y el certificado se
regenera solo para cubrir la direccion nueva.

Para ver la direccion de hoy sin reiniciar nada:

```powershell
.\scripts\estado-servicio.ps1
```

## Si desde el celular no carga

Casi siempre es el Firewall de Windows: cuando la red Wi-Fi esta clasificada
como **Publica**, Windows descarta las conexiones entrantes. El sintoma es
caracteristico: en esta PC la pagina abre bien, y en el celular se queda
cargando hasta agotar el tiempo (no dice "conexion rechazada").

Abre PowerShell **como administrador** y ejecuta una sola vez:

```powershell
.\scripts\abrir-firewall.ps1
```

## Primer arranque

```powershell
Copy-Item .env.example .env
notepad .env
.\scripts\iniciar-servicio.ps1
```

## HTTPS local

La app crea automáticamente un certificado local en `certs/`. El navegador puede mostrar una advertencia porque no es un certificado público de internet. Para red local esto es normal.

## Datos

- Archivos: `storage/`
- Eventos y estadísticas: `data/events.ndjson`
- Configuración: `.env`
- Sesiones persistentes: `data/sessions.json`

## Subir carpetas

En la barra superior usa el boton de carpeta para seleccionar una carpeta completa. La app conserva la estructura interna, por ejemplo:

```text
Fotos/Viaje/imagen.jpg
```

se guarda como:

```text
storage/Fotos/Viaje/imagen.jpg
```

La app no bloquea extensiones de programacion: puedes subir proyectos Node, React, Vite, Tailwind, .NET, Python, SQL y similares. El limite por archivo queda en 100 GB por defecto. Para proyectos con miles de archivos (`node_modules`, `.venv`, `bin`, `obj`, `__pycache__`) la app sube en cola para evitar saturar el navegador y Windows.

## Seguridad

La app usa clave local, cookie HTTP-only, bloqueo de rutas fuera de `storage/`, límite de tamaño por archivo y límite de subidas simultáneas.
