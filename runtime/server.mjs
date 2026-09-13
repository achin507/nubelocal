import crypto from 'node:crypto'
import fs from 'node:fs'
import fsp from 'node:fs/promises'
import http from 'node:http'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import cookieParser from 'cookie-parser'
import dotenv from 'dotenv'
import express from 'express'
import multer from 'multer'

dotenv.config()

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const projectRoot = path.resolve(__dirname, '..')
const cfg = {
  host: process.env.HOST || '0.0.0.0',
  port: Number(process.env.PORT || 8443),
  password: process.env.APP_PASSWORD || 'cambiame-local',
  storageDir: path.resolve(projectRoot, process.env.STORAGE_DIR || 'storage'),
  dataDir: path.resolve(projectRoot, process.env.DATA_DIR || 'data'),
  maxUploadBytes: Number(process.env.MAX_UPLOAD_BYTES || 10 * 1024 * 1024 * 1024),
  maxConcurrentUploads: Number(process.env.MAX_CONCURRENT_UPLOADS || 4),
  sessionHours: Number(process.env.SESSION_HOURS || 12),
  allowedLanPrefix: process.env.ALLOWED_LAN_PREFIX || '',
}

await fsp.mkdir(cfg.storageDir, { recursive: true })
await fsp.mkdir(cfg.dataDir, { recursive: true })
await ensureEnv()

const app = express()
const sessions = new Map()
let activeUploads = 0

app.disable('x-powered-by')
app.use((req, res, next) => {
  res.setHeader('X-Content-Type-Options', 'nosniff')
  res.setHeader('X-Frame-Options', 'DENY')
  res.setHeader('Referrer-Policy', 'no-referrer')
  next()
})
app.use(express.json({ limit: '8kb' }))
app.use(cookieParser())

const upload = multer({
  storage: multer.diskStorage({
    destination: async (req, file, cb) => {
      try {
        const target = safePath(String(req.query.path || ''))
        cb(null, target.full)
      } catch (error) {
        cb(error)
      }
    },
    filename: (req, file, cb) => {
      const name = path.basename(file.originalname)
      if (invalidName(name)) {
        cb(new Error('invalid file name'))
        return
      }
      cb(null, name)
    },
  }),
  limits: { fileSize: cfg.maxUploadBytes },
})

app.post('/api/login', async (req, res) => {
  if (!allowedNetwork(req)) {
    await appendEvent({ type: 'blocked', ip: clientIp(req), message: 'network prefix blocked' })
    res.status(403).send('forbidden')
    return
  }
  if (!constantPasswordEqual(String(req.body?.password || ''), cfg.password)) {
    await appendEvent({ type: 'login_failed', ip: clientIp(req) })
    res.status(403).send('forbidden')
    return
  }
  const token = crypto.randomBytes(32).toString('hex')
  const expiresAt = new Date(Date.now() + cfg.sessionHours * 60 * 60 * 1000)
  sessions.set(token, { expiresAt, ip: clientIp(req) })
  res.cookie('nube_session', token, {
    httpOnly: true,
    sameSite: 'lax',
    expires: expiresAt,
    path: '/',
  })
  await appendEvent({ type: 'login', ip: clientIp(req) })
  res.json({ ok: true, expiresAt })
})

app.post('/api/logout', (req, res) => {
  if (req.cookies?.nube_session) sessions.delete(req.cookies.nube_session)
  res.clearCookie('nube_session', { path: '/' })
  res.json({ ok: true })
})

app.use('/api', requireAuth)

app.get('/api/session', (req, res) => {
  res.json({ ok: true, ip: clientIp(req) })
})

app.get('/api/files', async (req, res) => {
  const target = safePath(String(req.query.path || ''))
  const entries = await fsp.readdir(target.full, { withFileTypes: true })
  const items = await Promise.all(
    entries.map(async (entry) => {
      const full = path.join(target.full, entry.name)
      const stat = await fsp.stat(full)
      return {
        name: entry.name,
        path: joinWebPath(target.rel, entry.name),
        type: entry.isDirectory() ? 'folder' : 'file',
        size: stat.size,
        modified: stat.mtime,
      }
    }),
  )
  items.sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'folder' ? -1 : 1))
  res.json({ path: target.rel, items })
})

app.post('/api/folders', async (req, res) => {
  const parent = safePath(String(req.body?.path || ''))
  const name = String(req.body?.name || '').trim()
  if (invalidName(name)) {
    res.status(400).send('invalid folder name')
    return
  }
  const full = path.join(parent.full, name)
  ensureInsideRoot(full)
  await fsp.mkdir(full)
  const webPath = joinWebPath(parent.rel, name)
  await appendEvent({ type: 'folder_created', ip: clientIp(req), path: webPath, name })
  res.json({ ok: true, path: webPath })
})

app.post('/api/uploads', (req, res, next) => {
  if (activeUploads >= cfg.maxConcurrentUploads) {
    res.status(429).send('too many uploads')
    return
  }
  activeUploads += 1
  const started = performance.now()
  upload.array('file')(req, res, async (error) => {
    activeUploads -= 1
    if (error) {
      next(error)
      return
    }
    const durationMs = Math.max(1, Math.round(performance.now() - started))
    const files = []
    for (const file of req.files || []) {
      const target = safePath(String(req.query.path || ''))
      const webPath = joinWebPath(target.rel, file.filename)
      const speedBps = file.size / (durationMs / 1000)
      const event = {
        type: 'upload',
        ip: clientIp(req),
        path: webPath,
        name: file.filename,
        bytes: file.size,
        durationMs,
        speedBps,
      }
      await appendEvent(event)
      files.push({ name: file.filename, path: webPath, bytes: file.size, speedBps })
    }
    res.json({ ok: true, files })
  })
})

app.get('/api/download', async (req, res) => {
  const target = safePath(String(req.query.path || ''))
  const stat = await fsp.stat(target.full)
  if (stat.isDirectory()) {
    res.status(404).send('not found')
    return
  }
  await appendEvent({ type: 'download', ip: clientIp(req), path: target.rel, name: path.basename(target.full), bytes: stat.size })
  res.download(target.full)
})

app.get('/api/stats', async (req, res) => {
  const events = await readEvents()
  const stats = { totalUploads: 0, totalDownloads: 0, totalBytes: 0, recent: events.slice(-50).reverse(), byIp: {} }
  for (const event of events) {
    const ip = event.ip || 'unknown'
    stats.byIp[ip] ||= { uploads: 0, bytes: 0, logins: 0 }
    if (event.type === 'upload') {
      stats.totalUploads += 1
      stats.totalBytes += event.bytes || 0
      stats.byIp[ip].uploads += 1
      stats.byIp[ip].bytes += event.bytes || 0
    }
    if (event.type === 'download') stats.totalDownloads += 1
    if (event.type === 'login') stats.byIp[ip].logins += 1
  }
  res.json(stats)
})

const dist = path.join(projectRoot, 'web', 'dist')
app.use('/assets', express.static(path.join(dist, 'assets'), { fallthrough: false }))
app.get(['/', '/transferencia', '/transferencia/*path'], (req, res) => {
  res.sendFile(path.join(dist, 'index.html'))
})

app.use((error, req, res, next) => {
  console.error(error)
  res.status(error.status || 400).send(error.message || 'request failed')
})

const server = http.createServer(app)
server.listen(cfg.port, cfg.host, () => {
  console.log(`Nube LAN Pro activa: http://localhost:${cfg.port}/transferencia`)
  console.log(`Desde tu celular/otra PC: http://TU-IP-LOCAL:${cfg.port}/transferencia`)
})

function requireAuth(req, res, next) {
  if (!allowedNetwork(req)) {
    res.status(403).send('forbidden')
    return
  }
  const token = req.cookies?.nube_session
  const session = token ? sessions.get(token) : null
  if (!session || session.expiresAt < new Date()) {
    if (token) sessions.delete(token)
    res.status(403).send('forbidden')
    return
  }
  next()
}

function safePath(input) {
  const clean = String(input || '').replaceAll('\\', '/').replace(/^\/+/, '')
  if (clean.includes('\0')) throw new Error('invalid path')
  const full = path.resolve(cfg.storageDir, clean)
  ensureInsideRoot(full)
  const rel = path.relative(cfg.storageDir, full).replaceAll('\\', '/')
  return { full, rel: rel === '.' ? '' : rel }
}

function ensureInsideRoot(full) {
  const rel = path.relative(cfg.storageDir, path.resolve(full))
  if (rel === '..' || rel.startsWith(`..${path.sep}`) || path.isAbsolute(rel)) {
    throw new Error('outside storage root')
  }
}

function invalidName(name) {
  return !name || name === '.' || name === '..' || /[\/\\:<>|?*"\0]/.test(name)
}

function joinWebPath(base, name) {
  return base ? `${base.replace(/^\/|\/$/g, '')}/${name}` : name
}

function clientIp(req) {
  return (req.socket.remoteAddress || '').replace('::ffff:', '')
}

function allowedNetwork(req) {
  return !cfg.allowedLanPrefix || clientIp(req).startsWith(cfg.allowedLanPrefix)
}

function constantPasswordEqual(a, b) {
  const left = crypto.createHash('sha256').update(a).digest()
  const right = crypto.createHash('sha256').update(b).digest()
  return crypto.timingSafeEqual(left, right)
}

async function appendEvent(event) {
  const row = JSON.stringify({ time: new Date().toISOString(), ...event })
  await fsp.appendFile(path.join(cfg.dataDir, 'events.ndjson'), `${row}\n`)
}

async function readEvents() {
  try {
    const raw = await fsp.readFile(path.join(cfg.dataDir, 'events.ndjson'), 'utf8')
    return raw
      .split('\n')
      .filter(Boolean)
      .map((line) => JSON.parse(line))
  } catch {
    return []
  }
}

async function ensureEnv() {
  const envPath = path.join(projectRoot, '.env')
  try {
    await fsp.access(envPath)
  } catch {
    await fsp.copyFile(path.join(projectRoot, '.env.example'), envPath)
  }
}

