package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*
var templateFS embed.FS

// ── types ────────────────────────────────────────────────────────────────────

type config struct{ Host, Port, Username, Password string }

type session struct {
	username  string
	expiresAt time.Time
}

type sessionStore struct {
	mu   sync.Mutex
	data map[string]session
}

type fileEntry struct {
	Name    string
	Size    int64
	ModTime time.Time
	IsDir   bool
	RelPath string // slash-separated, relative to rootDir; used in URL params
}

type browseData struct {
	Path        string
	Breadcrumbs []breadcrumb
	Entries     []fileEntry
	Error       string
}

type confirmData struct{ Path, ParentPath string }
type loginData struct{ Error string }
type breadcrumb struct{ Name, Path string }

// ── globals ───────────────────────────────────────────────────────────────────

var (
	cfg      config
	store    = sessionStore{data: make(map[string]session)}
	rootDir  string // abs path of cwd at startup
	selfPath string // abs path of this binary
	tmpl     *template.Template
)

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	if r, e := filepath.EvalSymlinks(exe); e == nil {
		exe = r
	}
	selfPath = filepath.Clean(exe)

	if rootDir, err = os.Getwd(); err != nil {
		log.Fatal(err)
	}
	rootDir = filepath.Clean(rootDir)

	envPath := filepath.Join(rootDir, ".env")
	if err = ensureEnv(envPath); err != nil {
		log.Fatalf("cannot write .env: %v", err)
	}
	if cfg, err = parseEnv(envPath); err != nil {
		log.Fatalf("cannot parse .env: %v", err)
	}

	tmpl = buildTemplates()

	go func() {
		for range time.NewTicker(15 * time.Minute).C {
			store.reap()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", handleLoginGet)
	mux.HandleFunc("POST /login", handleLoginPost)
	mux.HandleFunc("POST /logout", handleLogout)
	mux.HandleFunc("GET /download", handleDownload)
	mux.HandleFunc("GET /delete", handleDeleteGet)
	mux.HandleFunc("POST /delete", handleDeletePost)
	mux.HandleFunc("POST /upload", handleUpload)
	mux.HandleFunc("POST /upload/chunk", handleUploadChunk)
	mux.HandleFunc("POST /upload/cancel", handleUploadCancel)
	mux.HandleFunc("GET /", handleBrowse)

	addr := cfg.Host + ":" + cfg.Port
	log.Printf("filebrowser → http://%s   (ctrl+c to stop)", addr)
	log.Fatal(http.ListenAndServe(addr, logMiddleware(mux)))
}

// ── logging middleware ────────────────────────────────────────────────────────

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		ip := r.RemoteAddr
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			ip = fwd
		}
		log.Printf("%s  %-4s  %-30s  %d  %dB  %s",
			ip,
			r.Method,
			r.URL.RequestURI(),
			rec.status,
			rec.bytes,
			time.Since(start).Round(time.Microsecond),
		)
	})
}

// ── .env ──────────────────────────────────────────────────────────────────────

func ensureEnv(p string) error {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		const defaults = "HOST=0.0.0.0\nPORT=8080\nUSERNAME=admin\nPASSWORD=admin\n"
		return os.WriteFile(p, []byte(defaults), 0600)
	}
	return nil
}

func parseEnv(p string) (config, error) {
	c := config{Host: "0.0.0.0", Port: "8080", Username: "admin", Password: "admin"}
	data, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		val := strings.TrimSpace(kv[1])
		switch strings.ToUpper(strings.TrimSpace(kv[0])) {
		case "HOST":
			c.Host = val
		case "PORT":
			c.Port = val
		case "USERNAME":
			c.Username = val
		case "PASSWORD":
			c.Password = val
		}
	}
	return c, nil
}

// ── templates ─────────────────────────────────────────────────────────────────

func buildTemplates() *template.Template {
	funcs := template.FuncMap{
		"fileIcon":  fileIcon,
		"humanSize": humanSize,
		"fmtTime":   fmtTime,
		"urlq":      url.QueryEscape,
		"parentPath": func(p string) string {
			if par := path.Dir(p); par != "." {
				return par
			}
			return ""
		},
		"sub": func(a, b int) int { return a - b },
	}
	return template.Must(
		template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"),
	)
}

// ── session store ─────────────────────────────────────────────────────────────

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *sessionStore) create(user string) string {
	tok := newToken()
	s.mu.Lock()
	s.data[tok] = session{username: user, expiresAt: time.Now().Add(24 * time.Hour)}
	s.mu.Unlock()
	return tok
}

func (s *sessionStore) get(tok string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.data[tok]
	if !ok || time.Now().After(sess.expiresAt) {
		delete(s.data, tok)
		return session{}, false
	}
	return sess, true
}

func (s *sessionStore) del(tok string) {
	s.mu.Lock()
	delete(s.data, tok)
	s.mu.Unlock()
}

func (s *sessionStore) reap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.data {
		if now.After(v.expiresAt) {
			delete(s.data, k)
		}
	}
}

func authed(r *http.Request) bool {
	c, err := r.Cookie("session")
	if err != nil {
		return false
	}
	_, ok := store.get(c.Value)
	return ok
}

// ── path helpers ──────────────────────────────────────────────────────────────

// resolveSafePath resolves a user-supplied slash-path against rootDir,
// verifies it cannot escape via traversal, and returns the OS absolute path
// and the canonical slash-display path.
func resolveSafePath(rel string) (abs, display string, err error) {
	joined := filepath.Join(rootDir, filepath.FromSlash(rel))
	cleaned := filepath.Clean(joined)

	relToRoot, err := filepath.Rel(rootDir, cleaned)
	if err != nil {
		return "", "", fmt.Errorf("path error")
	}
	// Any ".." as first component means outside root
	if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path outside root")
	}

	disp := filepath.ToSlash(relToRoot)
	if disp == "." {
		disp = ""
	}
	return cleaned, disp, nil
}

func protected(abs string) bool {
	c := filepath.Clean(abs)
	return c == selfPath || c == filepath.Join(rootDir, ".env")
}

func buildCrumbs(disp string) []breadcrumb {
	crumbs := []breadcrumb{{Name: "~", Path: ""}}
	if disp == "" {
		return crumbs
	}
	parts := strings.Split(disp, "/")
	for i, p := range parts {
		crumbs = append(crumbs, breadcrumb{
			Name: p,
			Path: strings.Join(parts[:i+1], "/"),
		})
	}
	return crumbs
}

func resolveSafeFilePath(displayDir, name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid filename")
	}
	rel := name
	if displayDir != "" {
		rel = path.Join(displayDir, name)
	}
	abs, _, err := resolveSafePath(rel)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func sanitizeUploadFilename(name string) (string, error) {
	name = filepath.Base(filepath.FromSlash(name))
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("invalid filename")
	}
	return name, nil
}

// ── handlers ──────────────────────────────────────────────────────────────────

func handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if authed(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	tmpl.ExecuteTemplate(w, "login.html", loginData{})
}

func handleLoginPost(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	u, p := r.FormValue("username"), r.FormValue("password")
	uok := subtle.ConstantTimeCompare([]byte(u), []byte(cfg.Username)) == 1
	pok := subtle.ConstantTimeCompare([]byte(p), []byte(cfg.Password)) == 1
	if uok && pok {
		tok := store.create(u)
		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   86400,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	tmpl.ExecuteTemplate(w, "login.html", loginData{Error: "Invalid username or password."})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		store.del(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "session", MaxAge: -1, Path: "/"})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func handleBrowse(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	abs, disp, err := resolveSafePath(r.URL.Query().Get("path"))
	if err != nil {
		tmpl.ExecuteTemplate(w, "browse.html", browseData{Error: "Invalid path."})
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		tmpl.ExecuteTemplate(w, "browse.html", browseData{Error: "Directory not found."})
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		tmpl.ExecuteTemplate(w, "browse.html", browseData{Error: "Cannot read directory."})
		return
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	files := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		rel := e.Name()
		if disp != "" {
			rel = disp + "/" + e.Name()
		}
		files = append(files, fileEntry{
			Name: e.Name(), Size: info.Size(),
			ModTime: info.ModTime(), IsDir: e.IsDir(), RelPath: rel,
		})
	}

	tmpl.ExecuteTemplate(w, "browse.html", browseData{
		Path:        disp,
		Breadcrumbs: buildCrumbs(disp),
		Entries:     files,
	})
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	abs, _, err := resolveSafePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, "Open failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	mt := mime.TypeByExtension(filepath.Ext(abs))
	if mt == "" {
		mt = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(abs)+`"`)
	http.ServeContent(w, r, filepath.Base(abs), fi.ModTime(), f)
}

func handleDeleteGet(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	abs, disp, err := resolveSafePath(r.URL.Query().Get("path"))
	if err != nil || protected(abs) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	parent := path.Dir(disp)
	if parent == "." {
		parent = ""
	}
	tmpl.ExecuteTemplate(w, "confirm.html", confirmData{Path: disp, ParentPath: parent})
}

func handleDeletePost(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	r.ParseForm()
	abs, disp, err := resolveSafePath(r.FormValue("path"))
	if err != nil {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	if protected(abs) {
		http.Error(w, "Forbidden: protected path", http.StatusForbidden)
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		http.Error(w, "Delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	parent := path.Dir(disp)
	if parent == "." {
		parent = ""
	}
	http.Redirect(w, r, "/?path="+url.QueryEscape(parent), http.StatusSeeOther)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return
	}
	absDir, dispDir, err := resolveSafePath(r.FormValue("dir"))
	if err != nil {
		http.Error(w, "Invalid directory", http.StatusBadRequest)
		return
	}
	fi, err := os.Stat(absDir)
	if err != nil || !fi.IsDir() {
		http.Error(w, "Not a directory", http.StatusBadRequest)
		return
	}
	for _, hdrs := range r.MultipartForm.File {
		for _, fh := range hdrs {
			name := filepath.Base(filepath.FromSlash(fh.Filename))
			if name == "" || name == "." || name == ".." {
				continue
			}
			dest := filepath.Join(absDir, name)
			if protected(dest) {
				continue
			}
			src, err := fh.Open()
			if err != nil {
				continue
			}
			dst, err := os.Create(dest)
			if err != nil {
				src.Close()
				continue
			}
			io.Copy(dst, src)
			dst.Close()
			src.Close()
		}
	}
	http.Redirect(w, r, "/?path="+url.QueryEscape(dispDir), http.StatusSeeOther)
}

func handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Invalid multipart request", http.StatusBadRequest)
		return
	}
	absDir, dispDir, err := resolveSafePath(r.FormValue("dir"))
	if err != nil {
		http.Error(w, "Invalid directory", http.StatusBadRequest)
		return
	}
	fi, err := os.Stat(absDir)
	if err != nil || !fi.IsDir() {
		http.Error(w, "Not a directory", http.StatusBadRequest)
		return
	}
	name, err := sanitizeUploadFilename(r.FormValue("name"))
	if err != nil {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	offset, err := strconv.ParseInt(r.FormValue("offset"), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "Invalid offset", http.StatusBadRequest)
		return
	}
	total, err := strconv.ParseInt(r.FormValue("total"), 10, 64)
	if err != nil || total <= 0 {
		http.Error(w, "Invalid total size", http.StatusBadRequest)
		return
	}
	chunk, _, err := r.FormFile("chunk")
	if err != nil {
		http.Error(w, "Missing chunk", http.StatusBadRequest)
		return
	}
	defer chunk.Close()

	dest, err := resolveSafeFilePath(dispDir, name)
	if err != nil {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	if filepath.Dir(dest) != absDir {
		http.Error(w, "Invalid destination", http.StatusBadRequest)
		return
	}
	if protected(dest) {
		http.Error(w, "Forbidden: protected path", http.StatusForbidden)
		return
	}
	tmp := dest + ".uploading"
	if offset == 0 {
		_ = os.Remove(tmp)
	}
	cur := int64(0)
	if st, err := os.Stat(tmp); err == nil {
		cur = st.Size()
	} else if !os.IsNotExist(err) {
		http.Error(w, "Cannot inspect upload temp file", http.StatusInternalServerError)
		return
	}
	if cur != offset {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error":   "Offset mismatch",
			"current": cur,
		})
		return
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		http.Error(w, "Cannot create upload temp file", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	n, err := io.Copy(f, chunk)
	if err != nil {
		http.Error(w, "Cannot write upload chunk", http.StatusInternalServerError)
		return
	}
	uploaded := offset + n
	if uploaded > total {
		http.Error(w, "Uploaded bytes exceed declared size", http.StatusBadRequest)
		return
	}
	done := uploaded >= total || r.FormValue("done") == "1"
	if done {
		if err := os.Rename(tmp, dest); err != nil {
			if _, statErr := os.Stat(dest); os.IsNotExist(statErr) {
				http.Error(w, "Cannot finalize upload", http.StatusInternalServerError)
				return
			} else if statErr != nil {
				http.Error(w, "Cannot finalize upload", http.StatusInternalServerError)
				return
			}
			// On platforms where rename cannot replace existing files, move the
			// old destination aside, promote temp file, and restore on failure.
			backup := dest + ".uploading.bak"
			if err2 := os.Rename(dest, backup); err2 != nil {
				http.Error(w, "Cannot finalize upload", http.StatusInternalServerError)
				return
			}
			if err2 := os.Rename(tmp, dest); err2 != nil {
				_ = os.Rename(backup, dest)
				http.Error(w, "Cannot finalize upload", http.StatusInternalServerError)
				return
			}
			_ = os.Remove(backup)
		}
		if _, err := os.Stat(dest); err != nil {
			http.Error(w, "Cannot finalize upload: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"uploaded": uploaded,
		"total":    total,
		"done":     done,
	})
}

func handleUploadCancel(w http.ResponseWriter, r *http.Request) {
	if !authed(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	absDir, dispDir, err := resolveSafePath(r.FormValue("dir"))
	if err != nil {
		http.Error(w, "Invalid directory", http.StatusBadRequest)
		return
	}
	name, err := sanitizeUploadFilename(r.FormValue("name"))
	if err != nil {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	dest, err := resolveSafeFilePath(dispDir, name)
	if err != nil {
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}
	if filepath.Dir(dest) != absDir {
		http.Error(w, "Invalid destination", http.StatusBadRequest)
		return
	}
	if protected(dest) {
		http.Error(w, "Forbidden: protected path", http.StatusForbidden)
		return
	}
	tmp := dest + ".uploading"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		http.Error(w, "Cannot cancel upload", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func humanSize(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for v := n / u; v >= u; v /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func fileIcon(e fileEntry) string {
	if e.IsDir {
		return "📁"
	}
	switch strings.ToLower(filepath.Ext(e.Name)) {
	case ".go":
		return "🐹"
	case ".py":
		return "🐍"
	case ".js", ".ts", ".jsx", ".tsx", ".mjs":
		return "📜"
	case ".html", ".htm":
		return "🌐"
	case ".css", ".scss", ".sass":
		return "🎨"
	case ".json", ".yaml", ".yml", ".toml", ".xml":
		return "⚙️"
	case ".md", ".txt", ".rst", ".log":
		return "📄"
	case ".pdf":
		return "📑"
	case ".jpg", ".jpeg", ".png", ".gif", ".svg", ".webp", ".ico", ".bmp", ".tiff":
		return "🖼️"
	case ".mp4", ".mkv", ".avi", ".mov", ".webm":
		return "🎬"
	case ".mp3", ".wav", ".flac", ".ogg", ".m4a":
		return "🎵"
	case ".zip", ".tar", ".gz", ".bz2", ".xz", ".7z", ".rar", ".zst":
		return "📦"
	case ".exe", ".msi", ".dmg", ".deb", ".rpm":
		return "⚙️"
	case ".sh", ".bat", ".ps1", ".cmd":
		return "💻"
	case ".db", ".sqlite", ".sqlite3", ".sql":
		return "🗄️"
	case ".env":
		return "🔒"
	case ".pem", ".crt", ".key", ".p12":
		return "🔑"
	default:
		return "📄"
	}
}
