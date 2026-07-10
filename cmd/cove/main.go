package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/golang-jwt/jwt/v5"
	tusd "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/filestore"
	"golang.org/x/crypto/bcrypt"
)

var (
	storageRoot     string
	jwtSecret       []byte
	passwordHash    []byte
	sizeCache       sync.Map // map[string]int64
	uploadSessions  sync.Map // uploadId → tempFilePath
)

type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"isDir"`
	ModTime time.Time `json:"modTime"`
}

type TrashEntry struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"isDir"`
	Deleted time.Time `json:"deleted"`
}

func main() {
	flag.StringVar(&storageRoot, "root", "/mnt/nas", "root storage directory")
	addr     := flag.String("addr", ":8080", "listen address")
	password := flag.String("password", "", "login password (required)")
	secret   := flag.String("secret", "", "JWT secret key")
	flag.Parse()

	if *password == "" {
		log.Fatal("❌  --password is required. Example: ./cove --password mysecretpassword")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("failed to hash password: %v", err)
	}
	passwordHash = hash

	if *secret != "" {
		jwtSecret = []byte(*secret)
	} else {
		jwtSecret = []byte(fmt.Sprintf("cove-%s-%d", *password, time.Now().UnixNano()))
	}

	absRoot, err := filepath.Abs(storageRoot)
	if err != nil {
		log.Fatalf("cannot resolve storage root: %v", err)
	}
	storageRoot = absRoot

	if err := os.MkdirAll(storageRoot, 0755); err != nil {
		log.Fatalf("cannot create storage root: %v", err)
	}

	uploadTemp := filepath.Join(storageRoot, ".uploads")
	if err := os.MkdirAll(uploadTemp, 0755); err != nil {
		log.Fatalf("cannot create upload temp dir: %v", err)
	}

	// Clean up stale partial uploads on startup and every hour.
	// Interrupted uploads leave .bin + .info file pairs in .uploads/ that
	// accumulate silently and fill the disk. Anything older than 24h is dead.
	go func() {
		cleanStaleUploads := func() {
			entries, err := os.ReadDir(uploadTemp)
			if err != nil {
				return
			}
			cutoff := time.Now().Add(-24 * time.Hour)
			for _, e := range entries {
				info, err := e.Info()
				if err != nil {
					continue
				}
				if info.ModTime().Before(cutoff) {
					path := filepath.Join(uploadTemp, e.Name())
					os.Remove(path)
					log.Printf("cleaned stale upload: %s", e.Name())
				}
			}
		}
		cleanStaleUploads()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			cleanStaleUploads()
		}
	}()

	store    := filestore.New(uploadTemp)
	composer := tusd.NewStoreComposer()
	store.UseIn(composer)

	tusHandler, err := tusd.NewHandler(tusd.Config{
		BasePath:                "/api/upload/",
		StoreComposer:           composer,
		RespectForwardedHeaders: true,
		NotifyCompleteUploads:   true,
	})
	if err != nil {
		log.Fatalf("failed to create tus handler: %v", err)
	}

	go func() {
		for event := range tusHandler.CompleteUploads {
			info := event.Upload

			// currentPath is the folder the user was browsing
			currentPath := info.MetaData["path"]
			relativePath := info.MetaData["relativePath"]
			filename := filepath.Base(info.MetaData["filename"])
			if filename == "" || filename == "." {
				filename = info.ID
			}

			var destDir string
			if relativePath != "" && relativePath != filename {
				// Folder upload: relativePath is like "myfolder/sub/file.txt"
				// Preserve structure inside currentPath
				relDir := filepath.Dir(relativePath)
				destDir = filepath.Join(storageRoot, currentPath, relDir)
			} else {
				// Normal file upload: just put in currentPath
				destDir = filepath.Join(storageRoot, currentPath)
			}

			// Safety check
			if !strings.HasPrefix(destDir+string(filepath.Separator), storageRoot+string(filepath.Separator)) {
				log.Printf("unsafe dest path rejected: %s", destDir)
				continue
			}

			if err := os.MkdirAll(destDir, 0755); err != nil {
				log.Printf("mkdir error: %v", err)
				continue
			}

			src := filepath.Join(uploadTemp, info.ID)
			dst := filepath.Join(destDir, filename)

			if err := os.Rename(src, dst); err != nil {
				if err2 := copyFile(src, dst); err2 != nil {
					log.Printf("move error: %v", err2)
					continue
				}
				os.Remove(src)
			}
			os.Remove(src + ".info")
			sizeCache.Delete(destDir)
			sizeCache.Delete(filepath.Dir(destDir))
			log.Printf("upload complete: %s", dst)
		}
	}()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	// NOTE: middleware.Compress must NOT be applied globally — it wraps the
	// ResponseWriter with a gzip buffer that delays tus PATCH 204 responses,
	// causing the client to stall waiting for chunk acknowledgment.
	r.Use(corsMiddleware)

	r.Get("/login", serveLogin)
	r.Post("/api/auth/login", handleLogin)

	r.Group(func(r chi.Router) {
		r.Use(authMiddleware)
		r.Post("/api/auth/logout", handleLogout)
		r.Get("/api/ls", handleList)
		r.Get("/api/download", handleDownload)
		r.Get("/api/preview", handlePreview)
		r.Delete("/api/delete", handleDelete)
		r.Post("/api/mkdir", handleMkdir)
		r.Post("/api/move", handleMove)
		r.Get("/api/size", handleSize)
		r.Get("/api/diskusage", handleDiskUsage)
		r.Get("/api/trash", handleTrashList)
		r.Post("/api/trash/restore", handleTrashRestore)
		r.Delete("/api/trash/empty", handleTrashEmpty)
		r.Post("/api/upload-simple", handleSimpleUpload)
		r.Get("/api/upload-simple", handleUploadStatus)
		r.Post("/api/rename", handleRename)
		r.Mount("/api/upload/", http.StripPrefix("/api/upload/", tusHandler))
	})

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		if !isAuthenticated(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		http.ServeFile(w, r, "./web/index.html")
	})
	r.Handle("/*", authFileServer(http.Dir("./web")))

	srv := &http.Server{
		Addr:         *addr,
		Handler:      r,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  300 * time.Second, // keep connections alive longer for video streaming
	}

	log.Printf("🌊 Cove running at http://localhost%s", *addr)
	log.Printf("   Storage: %s", storageRoot)
	log.Fatal(srv.ListenAndServe())
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Password string `json:"password"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400); return
	}
	if err := bcrypt.CompareHashAndPassword(passwordHash, []byte(body.Password)); err != nil {
		http.Error(w, `{"error":"wrong password"}`, 401); return
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "cove",
		"exp": time.Now().Add(30 * 24 * time.Hour).Unix(),
	})
	signed, err := token.SignedString(jwtSecret)
	if err != nil { http.Error(w, "token error", 500); return }
	http.SetCookie(w, &http.Cookie{
		Name: "cove_token", Value: signed, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "cove_token", Value: "", Path: "/", MaxAge: -1})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie("cove_token")
	if err != nil { return false }
	token, err := jwt.Parse(cookie.Value, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return jwtSecret, nil
	})
	return err == nil && token.Valid
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAuthenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, `{"error":"unauthorized"}`, 401); return
			}
			http.Redirect(w, r, "/login", http.StatusFound); return
		}
		next.ServeHTTP(w, r)
	})
}

func authFileServer(fs http.FileSystem) http.Handler {
	fsh := http.FileServer(fs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAuthenticated(r) {
			http.Redirect(w, r, "/login", http.StatusFound); return
		}
		fsh.ServeHTTP(w, r)
	})
}

func serveLogin(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./web/login.html")
}

func safePath(rel string) (string, error) {
	rel = filepath.Clean("/" + rel)
	abs := filepath.Join(storageRoot, rel)
	if !strings.HasPrefix(abs+string(filepath.Separator), storageRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid path")
	}
	return abs, nil
}

func handleList(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if rel == "" { rel = "/" }
	abs, err := safePath(rel)
	if err != nil { http.Error(w, "invalid path", 400); return }
	entries, err := os.ReadDir(abs)
	if err != nil { http.Error(w, err.Error(), 500); return }
	var files []Entry
	for _, e := range entries {
		info, _ := e.Info()
		name := e.Name()
		if strings.HasPrefix(name, ".") { continue }
		files = append(files, Entry{
			Name: name, Path: filepath.Join(rel, name),
			Size: info.Size(), IsDir: e.IsDir(), ModTime: info.ModTime(),
		})
	}
	if files == nil { files = []Entry{} }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	abs, err := safePath(rel)
	if err != nil { http.Error(w, "invalid path", 400); return }
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(abs)+`"`)
	http.ServeFile(w, r, abs)
}

func handlePreview(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	abs, err := safePath(rel)
	if err != nil { http.Error(w, "invalid path", 400); return }

	ext := strings.ToLower(filepath.Ext(abs))

	// Cache media in browser for 1 hour — re-watching is instant, zero disk reads.
	switch ext {
	case ".mp4", ".mov", ".mkv", ".avi", ".webm", ".m4v",
		".mp3", ".flac", ".aac", ".wav", ".ogg", ".m4a",
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".bmp":
		w.Header().Set("Cache-Control", "private, max-age=3600")
	}

	// For video files, transparently serve the pre-transcoded H.264 compat
	// version if one exists. GoPro Hero 9+ and many cameras shoot H.265/HEVC,
	// which Chrome, Firefox, smart TVs and most non-Apple clients cannot decode.
	// transcodeCompatVideo runs in the background after every upload and produces
	// a universally compatible H.264 mp4 stored under .compat/. No client changes
	// needed — everyone gets smooth playback. ServeFile handles Range requests so
	// seeking and buffering work correctly.
	switch ext {
	case ".mp4", ".mov", ".m4v", ".mkv", ".avi":
		if cp := compatVideoPath(abs); cp != "" {
			if _, err := os.Stat(cp); err == nil {
				http.ServeFile(w, r, cp)
				return
			}
		}
	}

	http.ServeFile(w, r, abs)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	abs, err := safePath(rel)
	if err != nil { http.Error(w, "invalid path", 400); return }
	trash := trashDir()
	os.MkdirAll(trash, 0755)
	name := filepath.Base(abs)
	dest := filepath.Join(trash, fmt.Sprintf("%d__%s", time.Now().UnixMilli(), name))
	if err := os.Rename(abs, dest); err != nil {
		if err2 := copyFile(abs, dest); err2 != nil {
			http.Error(w, err2.Error(), 500); return
		}
		os.RemoveAll(abs)
	}
	sizeCache.Delete(abs)
	sizeCache.Delete(filepath.Dir(abs))
	w.WriteHeader(http.StatusNoContent)
}

func handleMkdir(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	abs, err := safePath(rel)
	if err != nil { http.Error(w, "invalid path", 400); return }
	if err := os.MkdirAll(abs, 0755); err != nil { http.Error(w, err.Error(), 500); return }
	w.WriteHeader(http.StatusCreated)
}

func handleMove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400); return
	}
	srcAbs, err := safePath(body.Src)
	if err != nil { http.Error(w, "invalid src", 400); return }
	dstAbs, err := safePath(body.Dst)
	if err != nil { http.Error(w, "invalid dst", 400); return }

	// If dst is a directory, move src into it
	if info, err := os.Stat(dstAbs); err == nil && info.IsDir() {
		dstAbs = filepath.Join(dstAbs, filepath.Base(srcAbs))
	}

	if err := os.MkdirAll(filepath.Dir(dstAbs), 0755); err != nil {
		http.Error(w, err.Error(), 500); return
	}
	if err := os.Rename(srcAbs, dstAbs); err != nil {
		http.Error(w, err.Error(), 500); return
	}
	sizeCache.Delete(srcAbs)
	sizeCache.Delete(filepath.Dir(srcAbs))
	sizeCache.Delete(filepath.Dir(dstAbs))
	w.WriteHeader(http.StatusNoContent)
}

func handleSize(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	abs, err := safePath(rel)
	if err != nil { http.Error(w, "invalid path", 400); return }
	if cached, ok := sizeCache.Load(abs); ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int64{"size": cached.(int64)})
		return
	}
	var size int64
	filepath.Walk(abs, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() { size += info.Size() }
		return nil
	})
	sizeCache.Store(abs, size)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"size": size})
}

func handleDiskUsage(w http.ResponseWriter, r *http.Request) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(storageRoot, &stat); err != nil {
		http.Error(w, err.Error(), 500); return
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free  := stat.Bavail * uint64(stat.Bsize)
	used  := total - free
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]uint64{"total": total, "used": used, "free": free})
}

func trashDir() string { return filepath.Join(storageRoot, ".trash") }

func handleTrashList(w http.ResponseWriter, r *http.Request) {
	trash := trashDir()
	os.MkdirAll(trash, 0755)
	entries, err := os.ReadDir(trash)
	if err != nil { http.Error(w, err.Error(), 500); return }
	var items []TrashEntry
	for _, e := range entries {
		info, _ := e.Info()
		name := e.Name()
		original := name
		var deletedAt time.Time
		if idx := strings.Index(name, "__"); idx > 0 {
			original = name[idx+2:]
			if ms, err := strconv.ParseInt(name[:idx], 10, 64); err == nil {
				deletedAt = time.UnixMilli(ms)
			}
		}
		items = append(items, TrashEntry{ID: name, Name: original, Size: info.Size(), IsDir: e.IsDir(), Deleted: deletedAt})
	}
	if items == nil { items = []TrashEntry{} }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

func handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID, Path string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400); return
	}
	src := filepath.Join(trashDir(), filepath.Base(body.ID))
	destDir, err := safePath(body.Path)
	if err != nil { http.Error(w, "invalid path", 400); return }
	name := body.ID
	if idx := strings.Index(name, "__"); idx > 0 { name = name[idx+2:] }
	if err := os.Rename(src, filepath.Join(destDir, name)); err != nil {
		http.Error(w, err.Error(), 500); return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleTrashEmpty(w http.ResponseWriter, r *http.Request) {
	os.RemoveAll(trashDir())
	os.MkdirAll(trashDir(), 0755)
	w.WriteHeader(http.StatusNoContent)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Upload-Length, Upload-Offset, Tus-Resumable, Upload-Metadata, Upload-Defer-Length, Upload-Concat, Location")
		w.Header().Set("Access-Control-Expose-Headers", "Upload-Offset, Location, Upload-Length, Tus-Version, Tus-Resumable, Tus-Max-Size, Tus-Extension, Upload-Metadata")
		if r.Method == "OPTIONS" { w.WriteHeader(http.StatusNoContent); return }
		next.ServeHTTP(w, r)
	})
}

// sanitizeUploadId strips everything except lowercase alphanumerics so a
// client-supplied id can never escape the uploads temp directory.
func sanitizeUploadId(id string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(id) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	s := b.String()
	if len(s) > 64 { s = s[:64] }
	return s
}

// handleUploadStatus returns how many bytes the server has received so far for
// a given upload session. The client calls this after aborting a paused upload
// to find the exact resume offset before sending the next request.
func handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	uploadId := sanitizeUploadId(r.URL.Query().Get("uploadId"))
	if uploadId == "" { http.Error(w, "uploadId required", 400); return }
	w.Header().Set("Content-Type", "application/json")
	val, ok := uploadSessions.Load(uploadId)
	if !ok {
		json.NewEncoder(w).Encode(map[string]int64{"offset": 0})
		return
	}
	info, err := os.Stat(val.(string))
	var size int64
	if err == nil { size = info.Size() }
	json.NewEncoder(w).Encode(map[string]int64{"offset": size})
}

// handleSimpleUpload streams a file body straight to disk in a single POST.
// Supports pause/resume: client supplies uploadId on first call; on resume it
// also supplies offset and the body starts from that byte position.
func handleSimpleUpload(w http.ResponseWriter, r *http.Request) {
	q            := r.URL.Query()
	currentPath  := q.Get("path")
	filename     := filepath.Base(q.Get("filename"))
	relativePath := q.Get("relativePath")
	uploadId     := sanitizeUploadId(q.Get("uploadId"))
	offsetStr    := q.Get("offset")
	sizeStr      := q.Get("size")

	if filename == "" || filename == "." {
		http.Error(w, "filename required", 400); return
	}

	var destDir string
	if relativePath != "" && relativePath != filename {
		destDir = filepath.Join(storageRoot, currentPath, filepath.Dir(relativePath))
	} else {
		destDir = filepath.Join(storageRoot, currentPath)
	}
	if !strings.HasPrefix(destDir+string(filepath.Separator), storageRoot+string(filepath.Separator)) {
		http.Error(w, "invalid path", 400); return
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		http.Error(w, err.Error(), 500); return
	}

	uploadTempDir := filepath.Join(storageRoot, ".uploads")
	var tmpPath string

	if uploadId == "" {
		http.Error(w, "uploadId required", 400); return
	}
	tmpPath = filepath.Join(uploadTempDir, "up-"+uploadId)

	if _, exists := uploadSessions.Load(uploadId); !exists {
		// First call for this uploadId — register the session
		uploadSessions.Store(uploadId, tmpPath)
	}

	var offset int64
	if offsetStr != "" {
		offset, _ = strconv.ParseInt(offsetStr, 10, 64)
	}

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil { http.Error(w, err.Error(), 500); return }
	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			f.Close(); http.Error(w, err.Error(), 500); return
		}
	}
	// Explicit large buffer cuts syscall overhead for big uploads — io.Copy's
	// default 32KB buffer means far more read/write syscalls than necessary
	// when writing to the exFAT-mounted storage drive.
	buf := make([]byte, 4*1024*1024)
	written, _ := io.CopyBuffer(f, r.Body, buf)
	f.Close()

	totalWritten := offset + written
	var fileSize int64
	if sizeStr != "" { fileSize, _ = strconv.ParseInt(sizeStr, 10, 64) }

	if fileSize > 0 && totalWritten < fileSize {
		// Partial upload — client can resume later
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"uploadId": uploadId, "offset": totalWritten,
		})
		return
	}

	// Complete — move temp file to destination
	uploadSessions.Delete(uploadId)
	dst := filepath.Join(destDir, filename)
	if err := os.Rename(tmpPath, dst); err != nil {
		if err2 := copyFile(tmpPath, dst); err2 != nil {
			http.Error(w, err2.Error(), 500); return
		}
		os.Remove(tmpPath)
	}
	sizeCache.Delete(destDir)
	sizeCache.Delete(filepath.Dir(destDir))
	log.Printf("upload complete: %s", dst)
	w.WriteHeader(http.StatusCreated)

	// Run post-upload video processing in the background, sequentially, so the
	// upload response returns immediately without blocking the client.
	//
	// faststartVideo       — moves the moov atom to the front so playback starts
	//                        instantly (applies to all video, fast remux via -c copy)
	// transcodeCompatVideo — produces a compatible/bitrate-capped H.264 copy under
	//                        .compat/ when needed (see its doc comment)
	//
	// These must run sequentially, not as two independent goroutines: faststartVideo
	// renames a new file over dst when it finishes, and if transcodeCompatVideo were
	// still mid-read of dst at that moment, the rename could corrupt its read
	// (observed as spurious ffmpeg failures on exFAT). Running compat transcoding
	// after faststart completes also means it reads the already-faststarted file.
	go func() {
		faststartVideo(dst)
		transcodeCompatVideo(dst)
	}()
}

// compatVideoPath returns the path where a pre-transcoded H.264 compat copy of
// a video file is stored. Compatible files live under storageRoot/.compat/ with
// the same relative path, suffixed with ".mp4". Returns "" if abs is not under
// storageRoot (safety check).
func compatVideoPath(abs string) string {
	rel, err := filepath.Rel(storageRoot, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.Join(storageRoot, ".compat", rel+".mp4")
}

// isHEVC returns true if the file at path is encoded with H.265/HEVC.
// We detect by running ffmpeg -i (which exits non-zero but prints codec info to
// stderr) and looking for "Video: hevc" in the output. This works without ffprobe.
func isHEVC(path string) bool {
	s := strings.ToLower(ffmpegProbe(path))
	return strings.Contains(s, "video: hevc") || strings.Contains(s, "video: h265")
}

// ffmpegProbe runs ffmpeg -i and returns its stderr+stdout output, which contains
// the stream info (codec, resolution, bitrate) even though the command itself
// always exits non-zero with no output file given.
func ffmpegProbe(path string) string {
	cmd := exec.Command("ffmpeg", "-i", path)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// videoBitrateKbps extracts the video stream's bitrate in kb/s from ffmpeg -i
// output, e.g. "Video: h264 ..., 45004 kb/s, 59.94 fps". Returns 0 if not found
// (container-level bitrate only, no per-stream figure — rare, but harmless: it
// just means the bitrate-cap check below won't trigger).
var videoBitrateRe = regexp.MustCompile(`Video:.*?(\d+) kb/s`)

func videoBitrateKbps(path string) int {
	m := videoBitrateRe.FindStringSubmatch(ffmpegProbe(path))
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// highBitrateKbps is the threshold above which even H.264 footage gets
// re-encoded down. GoPro "Protune"/high-bitrate modes and some phone footage
// shoot 1080p at 40-100+ Mbps — far more than needed for visually excellent
// playback, and more than most remote links (Tailscale, weak wifi, mobile
// hotspot) can sustain in real time, which is what causes stutter/rebuffering
// even though the codec itself is universally supported.
const highBitrateKbps = 20000

// transcodeCompatVideo produces a universally compatible, reasonably-sized
// H.264/AAC copy stored under storageRoot/.compat/, when either:
//   - the source is H.265/HEVC (Chrome, Firefox, smart TVs, most non-Apple
//     clients can't decode it), or
//   - the source is H.264 but shot at a very high bitrate (>20 Mbps) that
//     exceeds what most networks can stream in real time.
//
// This runs as a background goroutine after every upload, alongside faststartVideo.
// Once the compat file exists, handlePreview serves it instead of the original —
// transparently, with no client-side detection needed. The original file is
// never modified.
//
// Transcode settings:
//   -preset fast          : good speed/quality balance; Pi 5 handles 1080p in real time
//   -crf 23                : visually transparent quality for preview use
//   -maxrate/-bufsize      : caps peak bitrate so playback never outruns real-world
//                            network throughput, regardless of source bitrate
//   -vf scale              : cap at 1920px wide so 4K doesn't overwhelm the Pi at preview time
//   -movflags faststart    : moov atom at front so playback starts immediately
func transcodeCompatVideo(path string) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".mov", ".m4v", ".mkv", ".avi":
		// continue
	default:
		return
	}

	ffmpegCheckOnce.Do(func() {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			log.Printf("⚠️  ffmpeg not found — video compat transcoding disabled. Install with: sudo apt install ffmpeg")
			ffmpegMissing = true
		}
	})
	if ffmpegMissing {
		return
	}

	hevc := isHEVC(path)
	bitrate := videoBitrateKbps(path)
	if !hevc && bitrate <= highBitrateKbps {
		return
	}

	compatPath := compatVideoPath(path)
	if compatPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(compatPath), 0755); err != nil {
		log.Printf("compat mkdir failed for %s: %v", filepath.Base(path), err)
		return
	}

	// Skip if compat already exists (e.g. after a cove restart with existing files)
	if _, err := os.Stat(compatPath); err == nil {
		return
	}

	log.Printf("transcoding compat copy (hevc=%v bitrate=%dkbps): %s", hevc, bitrate, filepath.Base(path))
	// tmp doesn't end in .mp4, so ffmpeg can't infer the container format from
	// the extension alone — pass -f mp4 explicitly.
	tmp := compatPath + ".tmp"
	cmd := exec.Command("ffmpeg",
		"-i", path,
		"-c:v", "libx264",
		"-preset", "fast",
		"-crf", "23",
		"-maxrate", "12M",
		"-bufsize", "24M",
		"-vf", "scale='min(1920,iw)':-2", // cap at 1920px wide, preserve aspect ratio
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
		"-f", "mp4",
		"-y",
		tmp,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("compat transcode failed for %s: %v\n%s", filepath.Base(path), err, out)
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, compatPath); err != nil {
		log.Printf("compat transcode rename failed for %s: %v", filepath.Base(path), err)
		os.Remove(tmp)
		return
	}
	log.Printf("compat transcode done: %s", filepath.Base(path))
}

// faststartVideo runs ffmpeg -movflags +faststart on a video file, which moves
// the moov atom from the end of the file to the beginning. Without this, the
// browser must download the entire file before playback can start — that's the
// buffering. With faststart, the browser starts playing after receiving just
// the first few KB of metadata.
//
// iPhone .mov files always have moov at the end. mp4 files exported from
// iMovie/Photos are usually fine but not always. We run this on every video.
//
// ffmpeg must be installed: sudo apt install ffmpeg
// If not installed, this is a no-op — it logs once and skips.
var ffmpegMissing bool
var ffmpegCheckOnce sync.Once

func faststartVideo(path string) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".mov", ".m4v", ".mkv", ".avi":
		// continue
	default:
		return
	}

	ffmpegCheckOnce.Do(func() {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			log.Printf("⚠️  ffmpeg not found — video faststart disabled. Install with: sudo apt install ffmpeg")
			ffmpegMissing = true
		}
	})
	if ffmpegMissing { return }

	// tmp doesn't end in .mp4/.mov/etc, so ffmpeg can't infer the container
	// format from the extension alone — pass -f mp4 explicitly.
	tmp := path + ".faststart.tmp"
	cmd := exec.Command("ffmpeg",
		"-i", path,
		"-c", "copy",          // no re-encode — just reorder atoms, fast
		"-movflags", "+faststart",
		"-f", "mp4",
		"-y",                  // overwrite tmp without asking
		tmp,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("ffmpeg faststart failed for %s: %v\n%s", filepath.Base(path), err, out)
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("ffmpeg faststart rename failed: %v", err)
		os.Remove(tmp)
		return
	}
	log.Printf("faststart applied: %s", filepath.Base(path))
}

func handleRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400); return
	}
	newName := filepath.Base(body.NewName)
	if newName == "" || newName == "." {
		http.Error(w, "invalid name", 400); return
	}
	srcAbs, err := safePath(body.Path)
	if err != nil { http.Error(w, "invalid path", 400); return }
	dstAbs := filepath.Join(filepath.Dir(srcAbs), newName)
	if !strings.HasPrefix(dstAbs, storageRoot) {
		http.Error(w, "invalid path", 400); return
	}
	if err := os.Rename(srcAbs, dstAbs); err != nil {
		http.Error(w, err.Error(), 500); return
	}
	sizeCache.Delete(srcAbs)
	w.WriteHeader(http.StatusNoContent)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil { return err }
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil { return err }
	defer out.Close()
	_, err = out.ReadFrom(in)
	return err
}