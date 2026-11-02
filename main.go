// KwiqTest — a self-contained live quiz platform.
//
// The whole product is this one executable: HTTP server, realtime engine,
// storage, QR generation and report rendering, with the web frontend compiled
// in via go:embed. There is nothing to install, no database to provision and no
// internet connection required — copy the binary onto a laptop, run it, and
// everyone on the same network can join by scanning a code.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"kwiqtest/internal/api"
	"kwiqtest/internal/hub"
	"kwiqtest/internal/session"
	"kwiqtest/internal/store"
)

//go:embed all:web
var embedded embed.FS

// version is stamped at build time with -ldflags "-X main.version=…".
var version = "1.0.0"

func main() {
	var (
		addr       = flag.String("addr", ":37390", "listen address (host:port)")
		dataDir    = flag.String("data", "", "data directory (default: kwiqtest-data next to the binary)")
		publicBase = flag.String("public", "", "public base URL for join links and QR codes, e.g. https://quiz.example.com")
		ttlHours   = flag.Int("retain", 72, "hours to keep a session's data before it is deleted")
		maxImageMB = flag.Int("max-image", 5, "maximum image upload size in MB")
		maxVideoMB = flag.Int("max-video", 50, "maximum video upload size in MB")
		noBrowser  = flag.Bool("no-browser", false, "do not open a browser on start")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("KwiqTest %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	if *dataDir == "" {
		*dataDir = defaultDataDir()
	}
	abs, err := filepath.Abs(*dataDir)
	if err == nil {
		*dataDir = abs
	}

	retention := time.Duration(*ttlHours) * time.Hour
	st, err := store.Open(*dataDir, retention)
	if err != nil {
		log.Fatalf("could not open data directory %s: %v", *dataDir, err)
	}
	defer st.Close()

	web, err := fs.Sub(embedded, "web")
	if err != nil {
		log.Fatalf("embedded assets missing: %v", err)
	}

	hb := hub.New()
	eng := session.New(st, hb)
	defer eng.Close()

	cfg := api.DefaultConfig()
	cfg.SessionTTL = retention
	cfg.PublicBase = *publicBase
	cfg.MaxImageBytes = int64(*maxImageMB) << 20
	cfg.MaxVideoBytes = int64(*maxVideoMB) << 20

	srv := &http.Server{
		Addr:    *addr,
		Handler: logRequests(api.New(st, hb, eng, web, cfg).Routes()),
		// No WriteTimeout: SSE responses are deliberately long-lived, and a
		// write deadline would sever every participant mid-quiz.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("cannot listen on %s: %v\n\nIs another copy of KwiqTest already running? "+
			"Try a different port with -addr :8081", *addr, err)
	}

	urls := reachableURLs(ln.Addr().(*net.TCPAddr).Port, *publicBase)
	banner(urls, *dataDir)

	if !*noBrowser && len(urls) > 0 {
		go openBrowser(urls[0])
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		log.Fatalf("server error: %v", err)
	case <-stop:
	}

	fmt.Println("\nShutting down — saving session data…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	eng.Close()
	st.Close()
	fmt.Println("Saved. Goodbye.")
}

// defaultDataDir puts the data folder beside the executable so the binary plus
// its data travel together on a USB stick. It falls back to the working
// directory when the executable lives somewhere read-only.
func defaultDataDir() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Join(filepath.Dir(exe), "kwiqtest-data")
		if probe := filepath.Join(filepath.Dir(exe), ".kwiqtest-write-probe"); true {
			if f, err := os.Create(probe); err == nil {
				_ = f.Close()
				_ = os.Remove(probe)
				return dir
			}
		}
	}
	return "kwiqtest-data"
}

// reachableURLs lists the addresses participants can actually use. Getting this
// right matters more than it sounds: the organiser needs a URL that phones on
// the same Wi-Fi can reach, which is never "localhost".
func reachableURLs(port int, publicBase string) []string {
	var urls []string
	if publicBase != "" {
		urls = append(urls, strings.TrimRight(publicBase, "/"))
	}
	urls = append(urls, fmt.Sprintf("http://localhost:%d", port))
	ifaces, err := net.Interfaces()
	if err != nil {
		return urls
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
				continue
			}
			urls = append(urls, fmt.Sprintf("http://%s:%d", ipnet.IP.String(), port))
		}
	}
	return urls
}

func banner(urls []string, dataDir string) {
	line := strings.Repeat("─", 62)
	fmt.Printf("\n  KwiqTest %s — live quiz platform\n  %s\n\n", version, line)
	fmt.Println("  Open the organiser view here:")
	for i, u := range urls {
		tag := ""
		if i > 0 && strings.Contains(u, "localhost") {
			tag = "  (this computer only)"
		} else if i > 0 {
			tag = "  ← share this one with participants"
		}
		fmt.Printf("      %s%s\n", u, tag)
	}
	fmt.Printf("\n  Data folder: %s\n", dataDir)
	fmt.Printf("  Press Ctrl+C to stop.\n  %s\n\n", line)
}

// openBrowser launches the organiser's default browser. Failure is silent and
// harmless — the URL is already printed above.
func openBrowser(url string) {
	time.Sleep(350 * time.Millisecond)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// logRequests prints a one-line summary of failed requests only. Routine
// traffic — autosaves while editing, SSE polling, asset/media fetches — is
// silent so the console stays readable during a normal editing or hosting
// session; only genuine errors (4xx/5xx) are surfaced.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") ||
			strings.HasPrefix(r.URL.Path, "/assets/") ||
			strings.HasPrefix(r.URL.Path, "/media/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if rec.status >= 400 {
			log.Printf("%s %s → %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush keeps the SSE handler working through this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
