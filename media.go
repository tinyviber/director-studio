// media.go — 本地媒体文件服务。
//
// webview 里 <video>/<audio>/canvas drawImage 需要可 seek 的 URL。
// 起一个 127.0.0.1 随机端口服务，按绝对路径出文件（?p=/abs/path），
// 只允许 allow() 登记过的根目录下的媒体扩展名。Range 由
// http.ServeContent 原生处理（206/416 正确），拖进度条即真 seek。
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var mediaExt = map[string]bool{
	".wav": true, ".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".flac": true,
	".mp4": true, ".webm": true, ".mov": true, ".m4v": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true,
	// manifest.json 等配音产物元数据也要经媒体服务读给前端
	".json": true,
}

type mediaServer struct {
	base  string
	mu    sync.Mutex
	roots map[string]bool // 允许服务的根目录（绝对路径，cleaned）
}

func newMediaServer() (*mediaServer, error) {
	m := &mediaServer{roots: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/m", m.serve)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m.base = fmt.Sprintf("http://%s/m", ln.Addr().String())
	go http.Serve(ln, mux)
	return m, nil
}

func (m *mediaServer) allow(dir string) {
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	m.mu.Lock()
	m.roots[filepath.Clean(abs)] = true
	m.mu.Unlock()
}

func (m *mediaServer) allowed(p string) bool {
	if m.roots == nil {
		return false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)
	m.mu.Lock()
	defer m.mu.Unlock()
	for root := range m.roots {
		if abs == root || strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func (m *mediaServer) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("p")
	if p == "" || !filepath.IsAbs(p) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if !mediaExt[strings.ToLower(filepath.Ext(p))] {
		http.Error(w, "not media", http.StatusForbidden)
		return
	}
	if !m.allowed(p) {
		http.Error(w, "outside roots", http.StatusForbidden)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// ServeContent 处理 Range/If-Range/Content-Type 探测。
	http.ServeContent(w, r, filepath.Base(p), st.ModTime(), f)
}
