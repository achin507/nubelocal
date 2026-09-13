package main

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	urlpath "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Host             string
	Port             string
	Password         string
	StorageDir       string
	DataDir          string
	CertDir          string
	HTTPS            bool
	MaxUploadBytes   int64
	MaxConcurrent    int
	SessionHours     int
	AllowedLANPrefix string
}

type Server struct {
	cfg      Config
	root     string
	dataDir  string
	sessions map[string]Session
	mu       sync.Mutex
	eventsMu sync.Mutex
	uploads  chan struct{}
}

type Session struct {
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	IP        string    `json:"ip"`
}

type Event struct {
	Time       time.Time `json:"time"`
	Type       string    `json:"type"`
	IP         string    `json:"ip"`
	Path       string    `json:"path,omitempty"`
	Name       string    `json:"name,omitempty"`
	Bytes      int64     `json:"bytes,omitempty"`
	DurationMS int64     `json:"durationMs,omitempty"`
	SpeedBps   float64   `json:"speedBps,omitempty"`
	Message    string    `json:"message,omitempty"`
}

type FileItem struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Mime     string    `json:"mime,omitempty"`
}

type Stats struct {
	TotalUploads   int64             `json:"totalUploads"`
	TotalDownloads int64             `json:"totalDownloads"`
	TotalBytes     int64             `json:"totalBytes"`
	Recent         []Event           `json:"recent"`
	ByIP           map[string]IPStat `json:"byIp"`
}

type IPStat struct {
	Uploads int64 `json:"uploads"`
	Bytes   int64 `json:"bytes"`
	Logins  int64 `json:"logins"`
}

func main() {
	// Al arrancar en segundo plano, stdout va al log y stderr queda reservado
	// para caidas. Si el log saliera por stderr, PowerShell lo marca como error.
	log.SetOutput(os.Stdout)

	cfg := loadConfig()
	srv, err := NewServer(cfg)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	srv.routes(mux)

	addr := net.JoinHostPort(cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:              addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// Sin tope al cuerpo: con MAX_UPLOAD_BYTES en 100 GB, un ReadTimeout de
		// 30 minutos corta la subida a media transferencia y el navegador solo
		// dice "conexion fallida". La cabecera si tiene tope, que es lo que
		// protege de un cliente que abre y no manda nada.
		ReadTimeout:    0,
		WriteTimeout:   0,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 1 << 20,
	}

	if cfg.HTTPS {
		certFile, keyFile, err := ensureLocalCert(cfg.CertDir)
		if err != nil {
			log.Fatal(err)
		}
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		// HTTP/1.1 a proposito. Sobre TLS, Go negocia HTTP/2 solo, y en una
		// subida larga desde un celular el navegador corta el stream apenas la
		// pestana pasa a segundo plano: el servidor solo ve "client
		// disconnected", sin decir a que altura ni por que. Con HTTP/1.1 el
		// error llega como lo que es (conexion reiniciada, envio truncado).
		server.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
		logAccessURLs("https", cfg.Port)
		log.Fatal(server.ListenAndServeTLS(certFile, keyFile))
	}

	logAccessURLs("http", cfg.Port)
	log.Fatal(server.ListenAndServe())
}

func loadConfig() Config {
	env := readDotEnv(".env")
	get := func(key, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		if v := strings.TrimSpace(env[key]); v != "" {
			return v
		}
		return fallback
	}
	maxBytes, _ := strconv.ParseInt(get("MAX_UPLOAD_BYTES", "10737418240"), 10, 64)
	maxConcurrent, _ := strconv.Atoi(get("MAX_CONCURRENT_UPLOADS", "4"))
	sessionHours, _ := strconv.Atoi(get("SESSION_HOURS", "12"))
	https := strings.EqualFold(get("HTTPS", "true"), "true")
	password := get("APP_PASSWORD", "cambiame-local")
	return Config{
		Host:             get("HOST", "0.0.0.0"),
		Port:             get("PORT", "8443"),
		Password:         password,
		StorageDir:       get("STORAGE_DIR", "storage"),
		DataDir:          get("DATA_DIR", "data"),
		CertDir:          get("CERT_DIR", "certs"),
		HTTPS:            https,
		MaxUploadBytes:   maxBytes,
		MaxConcurrent:    max(1, maxConcurrent),
		SessionHours:     max(1, sessionHours),
		AllowedLANPrefix: get("ALLOWED_LAN_PREFIX", ""),
	}
}

func readDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		out[strings.TrimSpace(parts[0])] = strings.Trim(strings.TrimSpace(parts[1]), `"'`)
	}
	return out
}

func NewServer(cfg Config) (*Server, error) {
	root, err := filepath.Abs(cfg.StorageDir)
	if err != nil {
		return nil, err
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{root, dataDir, cfg.CertDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	s := &Server{
		cfg:      cfg,
		root:     root,
		dataDir:  dataDir,
		sessions: map[string]Session{},
		uploads:  make(chan struct{}, cfg.MaxConcurrent),
	}
	if err := s.loadSessions(); err != nil {
		log.Printf("session load failed: %v", err)
	}
	s.pruneExpiredSessions()
	return s, nil
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/session", s.requireAuth(s.handleSession))
	mux.HandleFunc("/api/files", s.requireAuth(s.handleFiles))
	mux.HandleFunc("/api/folders", s.requireAuth(s.handleFolders))
	mux.HandleFunc("/api/uploads", s.requireAuth(s.handleUploads))
	mux.HandleFunc("/api/download", s.requireAuth(s.handleDownload))
	mux.HandleFunc("/api/stats", s.requireAuth(s.handleStats))
	mux.HandleFunc("/", s.handleApp)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.allowedNetwork(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		s.appendEvent(Event{Time: time.Now(), Type: "blocked", IP: clientIP(r), Message: "network prefix blocked"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !constantPasswordEqual(body.Password, s.cfg.Password) {
		http.Error(w, "forbidden", http.StatusForbidden)
		s.appendEvent(Event{Time: time.Now(), Type: "login_failed", IP: clientIP(r)})
		return
	}
	token := randomToken()
	expires := time.Now().Add(time.Duration(s.cfg.SessionHours) * time.Hour)
	s.mu.Lock()
	s.sessions[token] = Session{CreatedAt: time.Now(), ExpiresAt: expires, IP: clientIP(r)}
	_ = s.saveSessionsLocked()
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "nube_session",
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.HTTPS,
	})
	s.appendEvent(Event{Time: time.Now(), Type: "login", IP: clientIP(r)})
	writeJSON(w, map[string]any{"ok": true, "expiresAt": expires})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("nube_session")
	if err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		_ = s.saveSessionsLocked()
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "nube_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.cfg.HTTPS})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "ip": clientIP(r)})
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := r.URL.Query().Get("path")
	full, relClean, err := s.safePath(rel)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	items := make([]FileItem, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		t := "file"
		if entry.IsDir() {
			t = "folder"
		}
		itemPath := joinWebPath(relClean, entry.Name())
		items = append(items, FileItem{
			Name:     entry.Name(),
			Path:     itemPath,
			Type:     t,
			Size:     info.Size(),
			Modified: info.ModTime(),
			Mime:     mime.TypeByExtension(filepath.Ext(entry.Name())),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return items[i].Type == "folder"
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	writeJSON(w, map[string]any{"path": relClean, "items": items})
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if invalidName(body.Name) {
		http.Error(w, "invalid folder name", http.StatusBadRequest)
		return
	}
	parent, relClean, err := s.safePath(body.Path)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	target := filepath.Join(parent, body.Name)
	if err := s.ensureInsideRoot(target); err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if err := os.Mkdir(target, 0755); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	path := joinWebPath(relClean, body.Name)
	s.appendEvent(Event{Time: time.Now(), Type: "folder_created", IP: clientIP(r), Path: path, Name: body.Name})
	writeJSON(w, map[string]any{"ok": true, "path": path})
}

func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	default:
		http.Error(w, "too many uploads", http.StatusTooManyRequests)
		return
	}

	targetPath := r.URL.Query().Get("path")
	dir, relClean, err := s.safePath(targetPath)
	if err != nil {
		s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Message: "bad target path"})
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	relativeUploadPath := r.URL.Query().Get("relativePath")
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Message: "multipart required"})
		http.Error(w, "multipart required", http.StatusBadRequest)
		return
	}
	type uploaded struct {
		Name     string  `json:"name"`
		Path     string  `json:"path"`
		Bytes    int64   `json:"bytes"`
		SpeedBps float64 `json:"speedBps"`
	}
	var result []uploaded
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			detalle := describeUploadError(err, nil, 0, s.cfg.MaxUploadBytes)
			log.Printf("subida interrumpida al leer el envio desde %s: %s", clientIP(r), detalle)
			s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Message: "interrumpida: " + detalle})
			http.Error(w, "upload interrupted: "+detalle, http.StatusBadRequest)
			return
		}
		if part.FileName() == "" {
			continue
		}
		name := filepath.Base(part.FileName())
		if invalidName(name) {
			s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Name: name, Message: "invalid file name"})
			http.Error(w, "invalid file name", http.StatusBadRequest)
			return
		}
		start := time.Now()
		finalPath, webPath, err := s.uploadDestination(dir, relClean, name, relativeUploadPath)
		if err != nil {
			s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Name: name, Message: "bad upload path"})
			http.Error(w, "bad upload path", http.StatusBadRequest)
			return
		}
		if err := s.ensureInsideRoot(finalPath); err != nil {
			s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Path: webPath, Name: name, Message: "outside storage root"})
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
			s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Path: webPath, Name: name, Message: "cannot create folders"})
			http.Error(w, "cannot create folders", http.StatusConflict)
			return
		}
		tmpPath := finalPath + ".uploading-" + randomToken()
		out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Path: webPath, Name: name, Message: "cannot create file"})
			http.Error(w, "cannot create file", http.StatusConflict)
			return
		}
		written, copyErr := io.CopyBuffer(out, part, make([]byte, 1024*1024))
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil {
			_ = os.Remove(tmpPath)
			// Guardar el motivo real. Antes se escribia "copy failed" a secas y
			// se perdia la causa: disco lleno, antivirus, limite de tamano o el
			// cliente que corto. Sin eso no hay forma de arreglar nada.
			detalle := describeUploadError(copyErr, closeErr, written, s.cfg.MaxUploadBytes)
			transcurrido := time.Since(start)
			velocidad := float64(written) / math.Max(0.001, transcurrido.Seconds())
			log.Printf("subida fallida: %s (%s) tras %s en %s a %s/s desde %s por %s",
				name, detalle, formatBytes(written), transcurrido.Round(time.Second),
				formatBytes(int64(velocidad)), clientIP(r), r.Proto)
			s.appendEvent(Event{
				Time:    time.Now(),
				Type:    "upload_failed",
				IP:      clientIP(r),
				Path:    webPath,
				Name:    name,
				Bytes:   written,
				Message: detalle,
			})
			http.Error(w, "upload failed: "+detalle, http.StatusBadRequest)
			return
		}
		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			s.appendEvent(Event{Time: time.Now(), Type: "upload_failed", IP: clientIP(r), Path: webPath, Name: name, Message: "rename failed"})
			http.Error(w, "cannot finish upload", http.StatusConflict)
			return
		}
		duration := time.Since(start)
		speed := safeSpeed(written, duration)
		s.appendEvent(Event{Time: time.Now(), Type: "upload", IP: clientIP(r), Path: webPath, Name: name, Bytes: written, DurationMS: duration.Milliseconds(), SpeedBps: speed})
		result = append(result, uploaded{Name: name, Path: webPath, Bytes: written, SpeedBps: speed})
	}
	writeJSON(w, map[string]any{"ok": true, "files": result})
}

func (s *Server) uploadDestination(baseDir, baseRel, fallbackName, relativePath string) (string, string, error) {
	cleanRel, err := cleanRelativeUploadPath(relativePath)
	if err != nil {
		return "", "", err
	}
	if cleanRel == "" {
		return filepath.Join(baseDir, fallbackName), joinWebPath(baseRel, fallbackName), nil
	}
	full := filepath.Join(baseDir, filepath.FromSlash(cleanRel))
	return full, joinWebPath(baseRel, filepath.ToSlash(filepath.FromSlash(cleanRel))), nil
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	// HEAD tambien: los gestores de descargas preguntan primero por el tamano y
	// el nombre antes de traerse el fichero entero. ServeFile lo resuelve solo.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	full, relClean, err := s.safePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	nombre := filepath.Base(full)

	// Sin esto el navegador se inventa el nombre. La URL es /api/download?path=...
	// y no lleva ninguno, asi que el unico sitio de donde puede sacarlo es esta
	// cabecera. Si falta, se lo deduce del Content-Type: un .rar servido como
	// application/x-compressed -que es lo que dice el registro de Windows en
	// cuanto hay un 7-Zip instalado- llega a la otra maquina llamandose .txz.
	w.Header().Set("Content-Disposition", disposicionAdjunto(nombre))

	// Y el tipo se fuerza a binario a proposito. El de la extension sale del
	// registro de Windows, cambia de maquina en maquina y no aporta nada para
	// descargar: lo unico que consigue es que el navegador renombre el fichero.
	// ServeFile respeta el Content-Type que ya este puesto.
	w.Header().Set("Content-Type", "application/octet-stream")

	s.appendEvent(Event{Time: time.Now(), Type: "download", IP: clientIP(r), Path: relClean, Name: nombre, Bytes: info.Size()})
	http.ServeFile(w, r, full)
}

// disposicionAdjunto arma la cabecera Content-Disposition con el nombre del
// fichero. Va dos veces: `filename` en ASCII para los clientes viejos y
// `filename*` codificado en UTF-8 (RFC 5987) para que las tildes y las enies
// lleguen enteras. Los navegadores actuales se quedan con el segundo.
func disposicionAdjunto(nombre string) string {
	// Un nombre de fichero no puede romper la cabecera ni colar otra detras, y
	// no todos los clientes entienden `filename*`. Se recorre una sola vez: se
	// quita lo peligroso y de paso se arma el respaldo en ASCII.
	var limpio, ascii strings.Builder
	for _, r := range nombre {
		if r == '"' || r == '\\' || r == '\r' || r == '\n' {
			continue
		}
		limpio.WriteRune(r)
		if r > 31 && r < 127 {
			ascii.WriteRune(r)
		} else {
			ascii.WriteByte('_')
		}
	}

	nombreLimpio := limpio.String()
	if nombreLimpio == "" {
		nombreLimpio = "descarga"
		ascii.Reset()
		ascii.WriteString("descarga")
	}

	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s",
		ascii.String(), url.PathEscape(nombreLimpio))
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := Stats{ByIP: map[string]IPStat{}}
	events, _ := s.readEvents()
	for _, ev := range events {
		ip := ev.IP
		if ip == "" {
			ip = "unknown"
		}
		ipStat := stats.ByIP[ip]
		switch ev.Type {
		case "upload":
			stats.TotalUploads++
			stats.TotalBytes += ev.Bytes
			ipStat.Uploads++
			ipStat.Bytes += ev.Bytes
		case "download":
			stats.TotalDownloads++
		case "login":
			ipStat.Logins++
		}
		stats.ByIP[ip] = ipStat
	}
	if len(events) > 50 {
		stats.Recent = events[len(events)-50:]
	} else {
		stats.Recent = events
	}
	for i, j := 0, len(stats.Recent)-1; i < j; i, j = i+1, j-1 {
		stats.Recent[i], stats.Recent[j] = stats.Recent[j], stats.Recent[i]
	}
	writeJSON(w, stats)
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	dist := filepath.Join("web", "dist")
	cleanURL := urlpath.Clean(r.URL.Path)
	if cleanURL == "/" || strings.HasPrefix(cleanURL, "/transferencia") {
		http.ServeFile(w, r, filepath.Join(dist, "index.html"))
		return
	}
	file := filepath.Join(dist, strings.TrimPrefix(cleanURL, "/"))
	if _, err := os.Stat(file); err == nil {
		http.ServeFile(w, r, file)
		return
	}
	http.ServeFile(w, r, filepath.Join(dist, "index.html"))
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedNetwork(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		c, err := r.Cookie("nube_session")
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.mu.Lock()
		session, ok := s.sessions[c.Value]
		if ok && time.Now().After(session.ExpiresAt) {
			delete(s.sessions, c.Value)
			_ = s.saveSessionsLocked()
			ok = false
		}
		s.mu.Unlock()
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *Server) loadSessions() error {
	path := filepath.Join(s.dataDir, "sessions.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var sessions map[string]Session
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return err
	}
	s.mu.Lock()
	s.sessions = sessions
	s.mu.Unlock()
	return nil
}

func (s *Server) pruneExpiredSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	now := time.Now()
	for token, session := range s.sessions {
		if now.After(session.ExpiresAt) {
			delete(s.sessions, token)
			changed = true
		}
	}
	if changed {
		_ = s.saveSessionsLocked()
	}
}

func (s *Server) saveSessionsLocked() error {
	path := filepath.Join(s.dataDir, "sessions.json")
	tmpPath := path + ".tmp"
	raw, err := json.MarshalIndent(s.sessions, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (s *Server) safePath(input string) (string, string, error) {
	clean := strings.ReplaceAll(input, "\\", "/")
	clean = strings.TrimPrefix(clean, "/")
	if clean == "." {
		clean = ""
	}
	if strings.Contains(clean, "\x00") {
		return "", "", errors.New("invalid path")
	}
	full := filepath.Join(s.root, filepath.FromSlash(clean))
	if err := s.ensureInsideRoot(full); err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(s.root, full)
	if err != nil {
		return "", "", err
	}
	if rel == "." {
		rel = ""
	}
	return full, filepath.ToSlash(rel), nil
}

func (s *Server) ensureInsideRoot(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("outside storage root")
	}
	return nil
}

func (s *Server) allowedNetwork(r *http.Request) bool {
	if s.cfg.AllowedLANPrefix == "" {
		return true
	}
	return strings.HasPrefix(clientIP(r), s.cfg.AllowedLANPrefix)
}

func (s *Server) appendEvent(ev Event) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	path := filepath.Join(s.dataDir, "events.ndjson")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("event write failed: %v", err)
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(ev)
}

func (s *Server) readEvents() ([]Event, error) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	path := filepath.Join(s.dataDir, "events.ndjson")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		var ev Event
		if json.Unmarshal(scanner.Bytes(), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events, scanner.Err()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("json response failed: %v", err)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func joinWebPath(base, name string) string {
	if base == "" {
		return name
	}
	return strings.Trim(base, "/") + "/" + name
}

func invalidName(name string) bool {
	name = strings.TrimSpace(name)
	return name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:<>|?*"`) || strings.Contains(name, "\x00")
}

func cleanRelativeUploadPath(input string) (string, error) {
	input = strings.TrimSpace(strings.ReplaceAll(input, "\\", "/"))
	input = strings.TrimPrefix(input, "/")
	if input == "" {
		return "", nil
	}
	if strings.Contains(input, "\x00") {
		return "", errors.New("invalid relative path")
	}
	parts := strings.Split(input, "/")
	for _, part := range parts {
		if invalidName(part) {
			return "", errors.New("invalid relative path segment")
		}
	}
	return strings.Join(parts, "/"), nil
}

func safeSpeed(bytes int64, duration time.Duration) float64 {
	if bytes <= 0 || duration <= 0 {
		return 0
	}
	seconds := duration.Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(bytes) / seconds
}

func constantPasswordEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func ensureLocalCert(certDir string) (string, string, error) {
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return "", "", err
	}
	certFile := filepath.Join(certDir, "local-cert.pem")
	keyFile := filepath.Join(certDir, "local-key.pem")
	if _, err := os.Stat(certFile); err == nil {
		if _, err := os.Stat(keyFile); err == nil {
			// El certificado solo sirve mientras cubra la IP que tiene la maquina
			// ahora. Si el router entrego otra por DHCP hay que rehacerlo, o el
			// navegador del celular rechaza la conexion contra la IP nueva.
			if certCoversLocalIPs(certFile) {
				return certFile, keyFile, nil
			}
			log.Printf("La IP local cambio y el certificado ya no la cubre: generando uno nuevo")
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Nube LAN Pro Local"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	addLocalIPs(&template)
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	certOut, err := os.Create(certFile)
	if err != nil {
		return "", "", err
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		_ = certOut.Close()
		return "", "", err
	}
	_ = certOut.Close()
	keyOut, err := os.OpenFile(keyFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", "", err
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		_ = keyOut.Close()
		return "", "", err
	}
	_ = keyOut.Close()
	return certFile, keyFile, nil
}

func addLocalIPs(cert *x509.Certificate) {
	cert.IPAddresses = append(cert.IPAddresses, localIPs()...)
}

// localIPs devuelve las direcciones de las interfaces activas, sin loopback.
func localIPs() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

// lanIPs deja solo las IPv4 utiles para escribir en el celular: descarta IPv6,
// las 169.254.x.x de un adaptador sin DHCP y la 192.168.56.1 de VirtualBox.
func lanIPs() []net.IP {
	var out []net.IP
	for _, ip := range localIPs() {
		v4 := ip.To4()
		if v4 == nil || v4.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, v4)
	}
	return out
}

// primaryLANIP es la IP por la que esta maquina sale a la red: la que veran los
// demas dispositivos. El Dial de UDP no manda ningun paquete, solo hace que el
// sistema elija la interfaz de salida.
func primaryLANIP() net.IP {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
			if v4 := addr.IP.To4(); v4 != nil {
				return v4
			}
		}
	}
	if ips := lanIPs(); len(ips) > 0 {
		return ips[0]
	}
	return nil
}

// certCoversLocalIPs dice si el certificado ya emitido sirve para la IP actual.
func certCoversLocalIPs(certFile string) bool {
	current := lanIPs()
	if len(current) == 0 {
		return true
	}
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if time.Now().After(cert.NotAfter) {
		return false
	}
	for _, ip := range current {
		found := false
		for _, san := range cert.IPAddresses {
			if san.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// logAccessURLs imprime la direccion real de hoy, no una escrita a mano.
func logAccessURLs(scheme, port string) {
	log.Printf("Nube LAN Pro lista en %s://localhost:%s/transferencia", scheme, port)
	primary := primaryLANIP()
	if primary != nil {
		log.Printf("Desde otra PC o celular: %s://%s:%s/transferencia", scheme, primary, port)
	}
	for _, ip := range lanIPs() {
		if primary != nil && ip.Equal(primary) {
			continue
		}
		log.Printf("Otra direccion posible:  %s://%s:%s/transferencia", scheme, ip, port)
	}
	if primary == nil {
		log.Printf("No encontre una IP de LAN: revisa que el Wi-Fi este conectado")
	}
}

// describeUploadError traduce el error de io.Copy a algo accionable. El codigo
// anterior descartaba esta informacion y dejaba solo "copy failed", que no
// distingue un disco lleno de un Wi-Fi que se cayo.
func describeUploadError(copyErr, closeErr error, written, maxBytes int64) string {
	err := copyErr
	if err == nil {
		err = closeErr
	}
	if err == nil {
		return "error desconocido"
	}

	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return fmt.Sprintf("el archivo supera el limite MAX_UPLOAD_BYTES (%s)", formatBytes(maxBytes))
	}

	texto := err.Error()
	lower := strings.ToLower(texto)

	switch {
	case strings.Contains(lower, "no space left"), strings.Contains(lower, "not enough space"),
		strings.Contains(lower, "disk is full"):
		return "el disco se quedo sin espacio"
	case errors.Is(err, os.ErrDeadlineExceeded), strings.Contains(lower, "timeout"):
		return "se agoto el tiempo de lectura del servidor (ReadTimeout)"
	case errors.Is(err, io.ErrUnexpectedEOF), strings.Contains(lower, "unexpected eof"):
		return "el envio se corto antes de terminar: el equipo que sube dejo de mandar datos"
	case strings.Contains(lower, "forcibly closed"), strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "broken pipe"), strings.Contains(lower, "wsarecv"),
		strings.Contains(lower, "wsasend"):
		return "la conexion se cerro de golpe: Wi-Fi, suspension del equipo o el navegador"
	case errors.Is(err, os.ErrPermission), strings.Contains(lower, "access is denied"),
		strings.Contains(lower, "being used by another process"):
		return "Windows bloqueo el archivo: antivirus o permisos"
	}

	return texto
}

func formatBytes(n int64) string {
	const unidad = 1024
	if n < unidad {
		return fmt.Sprintf("%d B", n)
	}
	div := int64(unidad)
	exp := 0
	for m := n / unidad; m >= unidad; m /= unidad {
		div *= unidad
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
