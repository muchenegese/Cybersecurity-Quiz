package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Compressing the app shell takes the first-load payload from roughly 90 KB to
// under 31 KB. That is the difference between a snappy join and a spinner when
// thirty phones hit the same link on conference-centre mobile data.

// minGzipSize is the point below which compression stops paying for itself:
// the gzip header and trailer alone are ~28 bytes, so small replies come out
// larger than they went in.
const minGzipSize = 1024

var gzipPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// compressible reports whether a content type benefits from gzip. Images,
// video and anything already compressed are left alone — re-compressing them
// costs CPU and gains nothing.
func compressible(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch ct {
	case "text/html", "text/css", "text/plain", "text/csv",
		"application/javascript", "text/javascript",
		"application/json", "application/manifest+json",
		"image/svg+xml":
		return true
	}
	return false
}

// gzipResponse defers the compress-or-not decision until it has seen enough of
// the body to know whether compression is worth it. Until then, writes are
// held in a small buffer.
type gzipResponse struct {
	http.ResponseWriter
	status    int
	sawHeader bool // the handler has chosen a status
	decided   bool // compress-or-not has been settled
	gz        *gzip.Writer
	buf       []byte
}

func (g *gzipResponse) WriteHeader(status int) {
	if g.sawHeader {
		return
	}
	g.sawHeader = true
	g.status = status

	h := g.Header()
	// Bodyless replies and anything already encoded are settled right away.
	if status == http.StatusNoContent || status == http.StatusNotModified ||
		h.Get("Content-Encoding") != "" || !compressible(h.Get("Content-Type")) {
		g.decide(false)
		return
	}
	// A declared length tells us up front whether it is worth compressing.
	if cl := h.Get("Content-Length"); cl != "" {
		n := 0
		for _, c := range cl {
			if c < '0' || c > '9' {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n >= 0 && n < minGzipSize {
			g.decide(false)
		}
	}
}

// decide settles the encoding, emits the status line, and releases anything
// buffered so far.
func (g *gzipResponse) decide(compress bool) {
	if g.decided {
		return
	}
	g.decided = true
	if !g.sawHeader {
		g.sawHeader = true
		g.status = http.StatusOK
	}
	if compress {
		h := g.Header()
		h.Set("Content-Encoding", "gzip")
		// The declared length describes the identity body, which is no longer
		// what we are sending. net/http will size the compressed reply itself.
		h.Del("Content-Length")
		h.Add("Vary", "Accept-Encoding")
		g.gz = gzipPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(g.status)
	if len(g.buf) > 0 {
		buffered := g.buf
		g.buf = nil
		g.writeOut(buffered)
	}
}

func (g *gzipResponse) writeOut(b []byte) (int, error) {
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipResponse) Write(b []byte) (int, error) {
	if !g.sawHeader {
		// Mirror net/http: an implicit 200 sniffs the type from the first write.
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.decided {
		return g.writeOut(b)
	}
	g.buf = append(g.buf, b...)
	if len(g.buf) >= minGzipSize {
		g.decide(true)
	}
	return len(b), nil
}

// Flush keeps streaming handlers working. A handler that flushes wants bytes
// on the wire now, so any pending decision is settled immediately.
func (g *gzipResponse) Flush() {
	if !g.decided {
		g.decide(len(g.buf) >= minGzipSize)
	}
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// close finishes the response, sending a short body uncompressed if the
// threshold was never reached.
func (g *gzipResponse) close() {
	if !g.decided {
		g.decide(false)
	}
	if g.gz != nil {
		_ = g.gz.Close()
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}

// withGzip compresses eligible responses.
//
// The SSE stream is excluded outright: it is a long-lived response where every
// frame must reach the client the instant it is written, and inserting a
// compressor into that path risks buffering a question push behind the
// deflate window for no bandwidth saving worth having.
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			strings.HasSuffix(r.URL.Path, "/events") ||
			strings.HasPrefix(r.URL.Path, "/media/") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponse{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}
