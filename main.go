package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "embed"
)

// Windows build (no console window):
// CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -s -w" -o "Obi Games Launcher.exe" .

//go:embed ui/index.html
var indexHTML []byte

//go:embed ui/logo.png
var logoPNG []byte

//go:embed ui/redline.jpg
var redlineJPG []byte

//go:embed ui/games.json
var gamesJSON []byte

type Game struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Tag   string   `json:"tag"`
	Blurb string   `json:"blurb"`
	Cover []string `json:"cover"`
	Image string   `json:"image,omitempty"`
	Exe   string   `json:"exe"`
}

var (
	mu          sync.Mutex
	catalog     []Game
	catalogPath string
	lastPing    atomic.Int64
)

func main() {
	setupLog()
	catalogPath = filepath.Join(exeDir(), "games.json")
	games, err := readCatalog(catalogPath, gamesJSON)
	if err != nil {
		fatal(fmt.Errorf("could not read games.json: %w", err))
	}
	catalog = games

	ln, err := net.Listen("tcp", "127.0.0.1:"+port())
	if err != nil {
		fatal(err)
	}
	go func() { _ = http.Serve(ln, routes()) }()

	url := "http://" + ln.Addr().String() + "/"
	if os.Getenv("OBI_NO_WINDOW") == "1" {
		fmt.Println(url)
		select {}
	}
	dir, err := openApp(url)
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(dir)
	watchClose()
}

func port() string {
	if p := os.Getenv("OBI_PORT"); p != "" {
		return p
	}
	return "0"
}

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /logo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(logoPNG)
	})
	mux.HandleFunc("GET /redline.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(redlineJPG)
	})
	mux.HandleFunc("GET /api/games", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, 200, catalog)
	})
	mux.HandleFunc("POST /api/ping", func(w http.ResponseWriter, r *http.Request) {
		lastPing.Store(time.Now().Unix())
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/games/exe", handleSetExe)
	mux.HandleFunc("POST /api/launch", handleLaunch)
	mux.HandleFunc("POST /api/browse", handleBrowse)
	return mux
}

func handleSetExe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  string `json:"id"`
		Exe string `json:"exe"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	path, err := safeExe(body.Exe)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range catalog {
		if catalog[i].ID != body.ID {
			continue
		}
		prev := catalog[i].Exe
		catalog[i].Exe = path
		// shortcut: saving a path rewrites games.json with the known fields only.
		if err := writeCatalog(catalogPath, catalog); err != nil {
			catalog[i].Exe = prev
			writeJSON(w, 500, map[string]string{"error": "Could not save games.json"})
			return
		}
		writeJSON(w, 200, map[string]string{"ok": "saved"})
		return
	}
	writeJSON(w, 404, map[string]string{"error": "Unknown game"})
}

func handleLaunch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	mu.Lock()
	var exe string
	var found bool
	for _, g := range catalog {
		if g.ID == body.ID {
			exe, found = g.Exe, true
			break
		}
	}
	mu.Unlock()
	if !found {
		writeJSON(w, 404, map[string]string{"error": "Unknown game"})
		return
	}
	path, err := safeExe(exe)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	cmd := exec.Command(path)
	cmd.Dir = filepath.Dir(path)
	if err := cmd.Start(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	go cmd.Wait()
	writeJSON(w, 200, map[string]string{"ok": "started"})
}

func handleBrowse(w http.ResponseWriter, r *http.Request) {
	path, err := browseExe()
	if err != nil {
		writeJSON(w, 501, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"path": path})
}

func browseExe() (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("Browse works on Windows. Paste the full path to the .exe.")
	}
	script := `Add-Type -AssemblyName System.Windows.Forms
$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Filter = 'Executable (*.exe)|*.exe'
$d.Title = 'Select the game executable'
$d.CheckFileExists = $true
if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $d.FileName }`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("Could not open the file picker.")
	}
	return strings.TrimSpace(string(out)), nil
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Bad request"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readCatalog(path string, fallback []byte) ([]Game, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			b = fallback
		} else {
			return nil, err
		}
	}
	var games []Game
	if err := json.Unmarshal(b, &games); err != nil {
		return nil, err
	}
	return games, nil
}

func writeCatalog(path string, games []Game) error {
	b, err := json.MarshalIndent(games, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 {
		q := p[0]
		if (q == '"' || q == '\'') && p[len(p)-1] == q {
			p = strings.TrimSpace(p[1 : len(p)-1])
		}
	}
	return p
}

func exeAllowed(goos, path string) bool {
	if goos == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	return true
}

func safeExe(p string) (string, error) {
	p = normalizePath(p)
	if p == "" || strings.ContainsAny(p, "\r\n") {
		return "", errors.New("Enter the full path to the game .exe")
	}
	if !exeAllowed(runtime.GOOS, p) {
		return "", errors.New("Choose an .exe file")
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", errors.New("Enter the full path to the game .exe")
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("That file was not found")
	}
	return p, nil
}

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Dir(p)
}

func setupLog() {
	if runtime.GOOS != "windows" {
		return
	}
	f, err := os.OpenFile(filepath.Join(exeDir(), "launcher.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	log.SetOutput(f)
}

func fatal(err error) {
	log.Println(err)
	if runtime.GOOS == "windows" {
		msg := strings.ReplaceAll(err.Error(), "'", "''")
		script := "Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.MessageBox]::Show('" + msg + "', 'Obi Games Launcher')"
		_ = exec.Command("powershell.exe", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", script).Run()
	}
	os.Exit(1)
}

// shortcut: the window is Edge or Chrome in app mode, not a built-in webview.
func openApp(url string) (string, error) {
	dir, err := os.MkdirTemp("", "obi-launcher-")
	if err != nil {
		return "", err
	}
	args := []string{
		"--app=" + url,
		"--window-size=1480,920",
		"--user-data-dir=" + dir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-sync",
	}
	var tried []string
	for _, bin := range browsers() {
		if st, err := os.Stat(bin); err != nil || st.IsDir() {
			continue
		}
		tried = append(tried, bin)
		cmd := exec.Command(bin, args...)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			continue
		}
		go cmd.Wait()
		return dir, nil
	}
	os.RemoveAll(dir)
	if len(tried) == 0 {
		return "", errors.New("Install Microsoft Edge or Google Chrome, then open the launcher again.")
	}
	return "", errors.New("Chrome or Edge did not start.")
}

func browsers() []string {
	if runtime.GOOS == "windows" {
		pf := os.Getenv("ProgramFiles")
		pf86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LocalAppData")
		return []string{
			filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
		}
	}
	return []string{
		"/usr/bin/google-chrome-stable",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	}
}

// shortcut: a missed ping means the window closed. The game process is not stopped.
func watchClose() {
	deadline := time.Now().Add(40 * time.Second)
	for {
		time.Sleep(time.Second)
		seen := lastPing.Load()
		if seen == 0 {
			if time.Now().After(deadline) {
				fatal(errors.New("The window did not open. Install Microsoft Edge or Google Chrome."))
			}
			continue
		}
		if time.Since(time.Unix(seen, 0)) > 7*time.Second {
			return
		}
	}
}
