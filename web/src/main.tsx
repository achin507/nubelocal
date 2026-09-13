import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'
import {
  BarChart3,
  Download,
  File,
  Folder,
  FolderPlus,
  FolderUp,
  HardDrive,
  Lock,
  LogOut,
  RefreshCw,
  Shield,
  UploadCloud,
} from 'lucide-react'
import './styles.css'

const MAX_BROWSER_UPLOADS = 4

type FileItem = {
  name: string
  path: string
  type: 'file' | 'folder'
  size: number
  modified: string
  mime?: string
}

type EventItem = {
  time: string
  type: string
  ip: string
  path?: string
  name?: string
  bytes?: number
  durationMs?: number
  speedBps?: number
}

type Stats = {
  totalUploads: number
  totalDownloads: number
  totalBytes: number
  recent: EventItem[]
  byIp: Record<string, { uploads: number; bytes: number; logins: number }>
}

type UploadState = {
  id: string
  name: string
  progress: number
  status: 'subiendo' | 'listo' | 'error'
  speed: number
  loaded: number
  size: number
}

type UploadFile = {
  file: File
  relativePath: string
}

type BrowserEntry = {
  name: string
  isFile: boolean
  isDirectory: boolean
}

type BrowserFileEntry = BrowserEntry & {
  file: (success: (file: File) => void, error: (error: DOMException) => void) => void
}

type BrowserDirectoryEntry = BrowserEntry & {
  createReader: () => {
    readEntries: (success: (entries: BrowserEntry[]) => void, error: (error: DOMException) => void) => void
  }
}

type DataTransferItemWithEntry = DataTransferItem & {
  webkitGetAsEntry?: () => BrowserEntry | null
}

type FolderInputProps = React.InputHTMLAttributes<HTMLInputElement> & {
  webkitdirectory?: string
  directory?: string
}

const folderPickerProps: FolderInputProps = {
  type: 'file',
  multiple: true,
  hidden: true,
  webkitdirectory: '',
  directory: '',
}

const api = async <T,>(url: string, options?: RequestInit): Promise<T> => {
  const response = await fetch(url, {
    credentials: 'same-origin',
    headers: options?.body instanceof FormData ? undefined : { 'Content-Type': 'application/json' },
    ...options,
  })
  if (!response.ok) {
    throw new Error(await response.text())
  }
  return response.json() as Promise<T>
}

function App() {
  const [loggedIn, setLoggedIn] = useState(false)
  const [checking, setChecking] = useState(true)
  const [password, setPassword] = useState('')
  const [path, setPath] = useState('')
  const [items, setItems] = useState<FileItem[]>([])
  const [stats, setStats] = useState<Stats | null>(null)
  const [uploads, setUploads] = useState<UploadState[]>([])
  const [dragging, setDragging] = useState(false)
  const [error, setError] = useState('')
  const inputRef = useRef<HTMLInputElement | null>(null)
  const folderInputRef = useRef<HTMLInputElement | null>(null)

  const uploadSummary = useMemo(() => {
    const total = uploads.length
    const done = uploads.filter((item) => item.status === 'listo').length
    const failed = uploads.filter((item) => item.status === 'error').length
    const active = uploads.filter((item) => item.status === 'subiendo').length
    const totalBytes = uploads.reduce((sum, item) => sum + item.size, 0)
    const loadedBytes = uploads.reduce((sum, item) => {
      if (item.status === 'listo') return sum + item.size
      if (item.status === 'error') return sum + item.loaded
      return sum + Math.min(item.loaded, item.size)
    }, 0)
    const progress = totalBytes > 0 ? Math.round((loadedBytes / totalBytes) * 100) : done > 0 && done === total ? 100 : 0
    return { total, done, failed, active, pending: Math.max(0, total - done - failed - active), totalBytes, loadedBytes, progress }
  }, [uploads])

  const breadcrumbs = useMemo(() => {
    const parts = path.split('/').filter(Boolean)
    return [{ label: 'storage', path: '' }].concat(
      parts.map((part, index) => ({ label: part, path: parts.slice(0, index + 1).join('/') })),
    )
  }, [path])

  const refresh = useCallback(async () => {
    const [files, nextStats] = await Promise.all([
      api<{ path: string; items: FileItem[] }>(`/api/files?path=${encodeURIComponent(path)}`),
      api<Stats>('/api/stats'),
    ])
    setItems(files.items)
    setStats(nextStats)
  }, [path])

  useEffect(() => {
    api('/api/session')
      .then(() => setLoggedIn(true))
      .catch(() => setLoggedIn(false))
      .finally(() => setChecking(false))
  }, [])

  useEffect(() => {
    if (loggedIn) {
      refresh().catch((err) => setError(err.message))
    }
  }, [loggedIn, refresh])

  const login = async (event: React.FormEvent) => {
    event.preventDefault()
    setError('')
    try {
      await api('/api/login', { method: 'POST', body: JSON.stringify({ password }) })
      setLoggedIn(true)
      setPassword('')
    } catch {
      setError('Clave incorrecta o acceso bloqueado.')
    }
  }

  const logout = async () => {
    await api('/api/logout', { method: 'POST' }).catch(() => undefined)
    setLoggedIn(false)
  }

  const uploadFiles = (files: FileList | File[] | UploadFile[]) => {
    const normalized: UploadFile[] = Array.from(files as ArrayLike<File | UploadFile>).map((item) =>
      isUploadFile(item) ? item : { file: item, relativePath: getRelativePath(item) },
    )

    if (normalized.length === 0) {
      setError('No encontre archivos para subir. Si es una carpeta, usa el boton de carpeta o arrastrala completa otra vez.')
      return
    }

    const folderFiles = normalized.filter((item) => item.relativePath.includes('/')).length
    if (folderFiles > 0) {
      setError(`Carpeta detectada: ${normalized.length} archivo(s), incluyendo subcarpetas.`)
    }

    uploadQueue(normalized)
  }

  const uploadQueue = async (files: UploadFile[]) => {
    let nextIndex = 0

    const worker = async () => {
      while (nextIndex < files.length) {
        const item = files[nextIndex]
        nextIndex += 1
        await uploadOneFile(item)
      }
    }

    await Promise.all(Array.from({ length: Math.min(MAX_BROWSER_UPLOADS, files.length) }, worker))
    refresh().catch((err) => setError(err.message))
  }

  const uploadOneFile = ({ file, relativePath }: UploadFile) => {
    return new Promise<void>((resolve) => {
      const label = relativePath || file.name
      const id = `${label}-${crypto.randomUUID()}`
      setUploads((current) => [{ id, name: label, progress: 0, status: 'subiendo', speed: 0, loaded: 0, size: file.size }, ...current])
      const form = new FormData()
      form.append('file', file)
      const xhr = new XMLHttpRequest()
      const started = performance.now()
      const params = new URLSearchParams({ path })
      if (relativePath) params.set('relativePath', relativePath)
      xhr.open('POST', `/api/uploads?${params.toString()}`)
      xhr.withCredentials = true
      xhr.upload.onprogress = (event) => {
        if (!event.lengthComputable) return
        const elapsed = Math.max(0.1, (performance.now() - started) / 1000)
        setUploads((current) =>
          current.map((item) =>
            item.id === id
              ? { ...item, progress: Math.round((event.loaded / event.total) * 100), speed: event.loaded / elapsed, loaded: event.loaded, size: event.total || item.size }
              : item,
          ),
        )
      }
      xhr.onload = () => {
        const ok = xhr.status >= 200 && xhr.status < 300
        setUploads((current) =>
          current.map((item) => (item.id === id ? { ...item, progress: ok ? 100 : item.progress, loaded: ok ? item.size : item.loaded, status: ok ? 'listo' : 'error' } : item)),
        )
        if (!ok) {
          setError(`No se pudo subir ${label}: ${xhr.responseText || `HTTP ${xhr.status}`}`)
        }
        resolve()
      }
      xhr.onerror = () => {
        setUploads((current) => current.map((item) => (item.id === id ? { ...item, status: 'error' } : item)))
        setError(`Conexion fallida subiendo ${label}.`)
        resolve()
      }
      xhr.send(form)
    })
  }

  const uploadDrop = async (dataTransfer: DataTransfer) => {
    setError('')
    const files = await filesFromDataTransfer(dataTransfer)
    uploadFiles(files)
  }

  const createFolder = async () => {
    const name = window.prompt('Nombre de la carpeta')
    if (!name) return
    setError('')
    try {
      await api('/api/folders', { method: 'POST', body: JSON.stringify({ path, name }) })
      await refresh()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'No se pudo crear la carpeta.')
    }
  }

  if (checking) {
    return <div className="center">Cargando Nube LAN Pro...</div>
  }

  if (!loggedIn) {
    return (
      <main className="loginShell">
        <section className="loginPanel">
          <div className="brandMark"><Shield size={28} /></div>
          <h1>Nube LAN Pro</h1>
          <p>Transferencia local rapida y privada para tu red de casa.</p>
          <form onSubmit={login}>
            <label htmlFor="password">Clave local</label>
            <div className="passwordRow">
              <Lock size={18} />
              <input
                id="password"
                type="password"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                autoFocus
                autoComplete="current-password"
              />
            </div>
            <button type="submit">Entrar</button>
          </form>
          {error && <p className="error">{error}</p>}
        </section>
      </main>
    )
  }

  return (
    <main className="app">
      <header className="topbar">
        <div>
          <p className="eyebrow">Servidor local</p>
          <h1>Nube LAN Pro</h1>
        </div>
        <div className="actions">
          <button onClick={() => refresh()} title="Actualizar"><RefreshCw size={18} /></button>
          <button onClick={createFolder} title="Crear carpeta"><FolderPlus size={18} /></button>
          <button onClick={() => inputRef.current?.click()} title="Subir archivos"><UploadCloud size={18} /></button>
          <button onClick={() => folderInputRef.current?.click()} title="Subir carpeta"><FolderUp size={18} /></button>
          <button onClick={logout} title="Salir"><LogOut size={18} /></button>
        </div>
      </header>

      <section
        className={`dropzone ${dragging ? 'active' : ''}`}
        onDragOver={(event) => {
          event.preventDefault()
          setDragging(true)
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(event) => {
          event.preventDefault()
          setDragging(false)
          uploadDrop(event.dataTransfer).catch((err) => setError(err instanceof Error ? err.message : 'No se pudo leer la carpeta.'))
        }}
      >
        <UploadCloud size={34} />
        <div>
          <strong>Arrastra archivos aqui o usa el boton de carpeta</strong>
          <span>Se guardan en /{path || 'storage'} y las carpetas conservan su estructura.</span>
        </div>
        <input ref={inputRef} type="file" multiple hidden onChange={(event) => event.target.files && uploadFiles(event.target.files)} />
        <input
          {...folderPickerProps}
          ref={folderInputRef}
          onChange={(event) => event.target.files && uploadFiles(event.target.files)}
        />
      </section>

      {error && <div className="notice">{error}</div>}

      <section className="layout">
        <div className="browserPanel">
          <nav className="breadcrumbs">
            {breadcrumbs.map((crumb) => (
              <button key={crumb.path || 'root'} onClick={() => setPath(crumb.path)}>
                {crumb.label}
              </button>
            ))}
          </nav>
          <div className="fileList">
            {items.length === 0 && <p className="empty">Esta carpeta esta vacia.</p>}
            {items.map((item) => (
              <div className="fileRow" key={item.path}>
                <button className="fileMain" onClick={() => item.type === 'folder' && setPath(item.path)}>
                  {item.type === 'folder' ? <Folder size={22} /> : <File size={22} />}
                  <span>
                    <strong>{item.name}</strong>
                    <small>{item.type === 'folder' ? 'Carpeta' : `${formatBytes(item.size)} · ${item.mime || 'archivo'}`}</small>
                  </span>
                </button>
                <div className="fileMeta">
                  <span>{new Date(item.modified).toLocaleString()}</span>
                  {item.type === 'file' && (
                    <a href={`/api/download?path=${encodeURIComponent(item.path)}`} title="Descargar">
                      <Download size={18} />
                    </a>
                  )}
                </div>
              </div>
            ))}
          </div>
        </div>

        <aside className="sidePanel">
          <div className="metricGrid">
            <Metric icon={<UploadCloud />} label="Subidas" value={stats?.totalUploads ?? 0} />
            <Metric icon={<Download />} label="Descargas" value={stats?.totalDownloads ?? 0} />
            <Metric icon={<HardDrive />} label="Transferido" value={formatBytes(stats?.totalBytes ?? 0)} />
            <Metric icon={<BarChart3 />} label="IPs" value={Object.keys(stats?.byIp ?? {}).length} />
          </div>

          <h2>Transferencias</h2>
          {uploadSummary.total > 0 && (
            <div className="uploadSummary">
              <div className="summaryTop">
                <strong>{uploadSummary.progress}%</strong>
                <span>{uploadSummary.done}/{uploadSummary.total} listos</span>
              </div>
              <progress value={uploadSummary.progress} max={100} />
              <div className="summaryMeta">
                <span>{formatBytes(uploadSummary.loadedBytes)} / {formatBytes(uploadSummary.totalBytes)}</span>
                <span>{uploadSummary.active} subiendo · {uploadSummary.pending} en cola · {uploadSummary.failed} error</span>
              </div>
            </div>
          )}
          <div className="uploads">
            {uploads.length === 0 && <p className="empty">Sin subidas activas.</p>}
            {uploads.map((upload) => (
              <div className="uploadItem" key={upload.id}>
                <div>
                  <strong title={upload.name}>{upload.name}</strong>
                  <small>{upload.status} · {formatBytes(upload.loaded)} / {formatBytes(upload.size)} · {formatBytes(upload.speed)}/s</small>
                </div>
                <progress value={upload.progress} max={100} />
              </div>
            ))}
          </div>

          <h2>Historial</h2>
          <div className="events">
            {(stats?.recent ?? []).slice(0, 10).map((event, index) => (
              <div className="eventItem" key={`${event.time}-${index}`}>
                <span>{event.type}</span>
                <strong>{event.name || event.path || event.ip}</strong>
                <small>{event.ip} · {event.bytes ? formatBytes(event.bytes) : 'sin bytes'} · {new Date(event.time).toLocaleTimeString()}</small>
              </div>
            ))}
          </div>
        </aside>
      </section>
    </main>
  )
}

function Metric({ icon, label, value }: { icon: React.ReactNode; label: string; value: React.ReactNode }) {
  return (
    <div className="metric">
      {icon}
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  )
}

function formatBytes(bytes: number) {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const power = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1)
  return `${(bytes / 1024 ** power).toFixed(power === 0 ? 0 : 1)} ${units[power]}`
}

function getRelativePath(file: File) {
  return (file as File & { webkitRelativePath?: string }).webkitRelativePath || ''
}

function isUploadFile(item: File | UploadFile): item is UploadFile {
  return typeof item === 'object' && item !== null && 'file' in item && 'relativePath' in item
}

async function filesFromDataTransfer(dataTransfer: DataTransfer): Promise<UploadFile[]> {
  const items = Array.from(dataTransfer.items || [])
  const entries: BrowserEntry[] = []
  items.forEach((item) => {
    const entry = (item as DataTransferItemWithEntry).webkitGetAsEntry?.()
    if (entry) entries.push(entry)
  })

  if (entries.length > 0) {
    const nested = await Promise.all(entries.map((entry) => collectEntryFiles(entry, '')))
    return nested.flat()
  }

  return Array.from(dataTransfer.files).map((file) => ({ file, relativePath: getRelativePath(file) }))
}

async function collectEntryFiles(entry: BrowserEntry, parentPath: string): Promise<UploadFile[]> {
  const entryPath = parentPath ? `${parentPath}/${entry.name}` : entry.name

  if (entry.isFile) {
    const file = await new Promise<File>((resolve, reject) => {
      ;(entry as BrowserFileEntry).file(resolve, reject)
    })
    return [{ file, relativePath: entryPath }]
  }

  if (!entry.isDirectory) return []

  const reader = (entry as BrowserDirectoryEntry).createReader()
  const children: BrowserEntry[] = []
  while (true) {
    const batch = await new Promise<BrowserEntry[]>((resolve, reject) => {
      reader.readEntries(resolve, reject)
    })
    if (batch.length === 0) break
    children.push(...batch)
  }

  const nested = await Promise.all(children.map((child) => collectEntryFiles(child, entryPath)))
  return nested.flat()
}

createRoot(document.getElementById('root')!).render(<App />)
