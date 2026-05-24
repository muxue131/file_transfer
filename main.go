package main

import (
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	uploadDir   = "./uploads"
	defaultPort = "8080"

	readHeaderTimeout = 10 * time.Second
	writeTimeout      = 30 * time.Minute
	idleTimeout       = 60 * time.Second
	maxUploadMemory   = 32 << 20
)

func main() {
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatalf("failed to create upload directory: %v", err)
	}

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	http.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(uploadDir))))
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/api/ip", ipHandler)
	http.HandleFunc("/api/files", filesHandler)
	http.HandleFunc("/api/delete", deleteHandler)
	http.HandleFunc("/", indexHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	host := getLocalIP()

	log.Printf("starting server on port %s (LAN: %s)", port, host)

	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser("http://localhost:" + port)
	}()

	srv := &http.Server{
		Addr:              ":" + port,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	log.Fatal(srv.ListenAndServe())
}

func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd = "open"
		args = []string{url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	if err := exec.Command(cmd, args...).Start(); err != nil {
		log.Printf("failed to open browser: %v", err)
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "./static/index.html")
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	reader, err := r.MultipartReader()
	if err != nil {
		log.Printf("multipart read error: %v", err)
		http.Error(w, "文件解析失败: "+err.Error(), http.StatusBadRequest)
		return
	}

	var part *multipart.Part
	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("multipart nextpart error: %v", err)
			http.Error(w, "读取文件数据失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		if p.FormName() == "file" {
			part = p
			break
		} else {
			p.Close()
		}
	}

	if part == nil {
		http.Error(w, "未找到文件字段", http.StatusBadRequest)
		return
	}

	fileName := part.FileName()
	if fileName == "" {
		part.Close()
		http.Error(w, "文件名为空", http.StatusBadRequest)
		return
	}

	safeName := filepath.Base(fileName)
	dstPath := filepath.Join(uploadDir, safeName)

	dst, err := os.Create(dstPath)
	if err != nil {
		part.Close()
		log.Printf("create file error: %v", err)
		http.Error(w, "创建文件失败", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	buf := make([]byte, maxUploadMemory)
	written, err := io.CopyBuffer(dst, part, buf)
	if err != nil {
		dst.Close()
		os.Remove(dstPath)
		part.Close()
		log.Printf("copy file error (written %d bytes): %v", written, err)
		http.Error(w, "写入文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	part.Close()

	if err := dst.Sync(); err != nil {
		log.Printf("sync file error: %v", err)
	}

	resp := map[string]interface{}{
		"url":  "/uploads/" + safeName,
		"name": fileName,
		"size": written,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode response error: %v", err)
	}
}

func getLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ip, ok := addr.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
				return ip.IP.String()
			}
		}
	}
	return "127.0.0.1"
}

func ipHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"localIP": getLocalIP(),
	})
}

func filesHandler(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(uploadDir, 0755); err != nil {
				log.Printf("create upload dir error: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"files": []interface{}{}})
			return
		}
		log.Printf("read directory error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"files": []interface{}{}})
		return
	}

	type FileInfo struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"url"`
		Type string `json:"type"`
	}

	list := make([]FileInfo, 0)
	for _, e := range entries {
		if !e.IsDir() {
			info, err := e.Info()
			if err != nil {
				list = append(list, FileInfo{Name: e.Name(), Size: 0, URL: "/uploads/" + e.Name(), Type: classifyFile(e.Name())})
				continue
			}
			list = append(list, FileInfo{Name: e.Name(), Size: info.Size(), URL: "/uploads/" + e.Name(), Type: classifyFile(e.Name())})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"files": list})
}

func classifyFile(name string) string {
	x := strings.ToLower(name)
	switch {
	case isImage(x):
		return "image"
	case isVideo(x):
		return "video"
	case isDocument(x):
		return "document"
	default:
		return "other"
	}
}

func isImage(x string) bool {
	for _, ext := range []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg", ".ico"} {
		if strings.HasSuffix(x, ext) { return true }
	}
	return false
}

func isVideo(x string) bool {
	for _, ext := range []string{".mp4", ".avi", ".mov", ".webm", ".mkv", ".flv", ".wmv"} {
		if strings.HasSuffix(x, ext) { return true }
	}
	return false
}

func isDocument(x string) bool {
	for _, ext := range []string{".pdf", ".doc", ".docx", ".ppt", ".pptx", ".txt", ".md", ".xls", ".xlsx", ".csv", ".rtf", ".odt"} {
		if strings.HasSuffix(x, ext) { return true }
	}
	return false
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "无效请求", http.StatusBadRequest)
		return
	}

	safeName := filepath.Base(req.Name)
	if safeName == "" || safeName == "." || safeName == ".." {
		http.Error(w, "无效文件名", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(uploadDir, safeName)
	if err := os.Remove(fullPath); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "文件不存在", http.StatusNotFound)
			return
		}
		log.Printf("delete file error: %v", err)
		http.Error(w, "删除失败", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}
