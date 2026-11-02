package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwiqtest/internal/hub"
	"kwiqtest/internal/model"
	"kwiqtest/internal/session"
	"kwiqtest/internal/store"
)

const testHostToken = "0123456789abcdef0123456789abcdef"

type harness struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, time.Hour)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.Close)

	hb := hub.New()
	eng := session.New(st, hb)
	t.Cleanup(eng.Close)

	web := os.DirFS(webFixture(t))
	s := New(st, hb, eng, web, DefaultConfig())
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, st: st}
}

// webFixture provides a stand-in index.html so the SPA handler has something
// to serve; the real assets are embedded in the binary and not visible to
// tests. It is padded past the gzip threshold so compression behaviour matches
// the shipped shell.
func webFixture(t *testing.T) string {
	dir := t.TempDir()
	shell := "<!doctype html><title>KwiqTest</title><body><div id=app></div>" +
		strings.Repeat("<!-- stand-in for the real application shell -->", 40)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(shell), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (h *harness) do(method, path string, body any, headers map[string]string) *http.Response {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, r)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Host-Token", testHostToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *harness) json(method, path string, body any, headers map[string]string, out any) *http.Response {
	h.t.Helper()
	res := h.do(method, path, body, headers)
	if out != nil {
		defer res.Body.Close()
		if err := json.NewDecoder(res.Body).Decode(out); err != nil && err != io.EOF {
			h.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return res
}

// seedQuiz creates a publishable two-question quiz and returns its id.
func (h *harness) seedQuiz() string {
	h.t.Helper()
	var q model.Quiz
	res := h.json("POST", "/api/quizzes", map[string]any{"title": "Geography"}, nil, &q)
	if res.StatusCode != http.StatusCreated {
		h.t.Fatalf("create quiz: status %d", res.StatusCode)
	}
	body := map[string]any{
		"questions": []model.Question{
			{
				Text: "Capital of France?", TimeLimitSec: 20, Points: 1000, CorrectIndex: 0,
				Options: []model.Option{{Text: "Paris"}, {Text: "Lyon"}, {Text: "Nice"}},
			},
			{
				Text: "Largest ocean?", TimeLimitSec: 15, Points: 1000, CorrectIndex: 1,
				Options: []model.Option{{Text: "Atlantic"}, {Text: "Pacific"}},
			},
		},
	}
	res = h.json("PUT", "/api/quizzes/"+q.ID, body, nil, nil)
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("update quiz: status %d", res.StatusCode)
	}
	return q.ID
}

func TestQuizOwnershipIsEnforced(t *testing.T) {
	h := newHarness(t)
	id := h.seedQuiz()

	res := h.do("GET", "/api/quizzes/"+id, nil, map[string]string{"X-Host-Token": "someone-elses-token-aaaaaaaaaaaaaaaa"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("foreign token reading a quiz got %d, want 404", res.StatusCode)
	}

	res = h.do("GET", "/api/quizzes/"+id, nil, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("owner reading own quiz got %d, want 200", res.StatusCode)
	}
}

func TestPublishRejectsIncompleteQuiz(t *testing.T) {
	h := newHarness(t)
	var q model.Quiz
	h.json("POST", "/api/quizzes", map[string]any{"title": "Empty"}, nil, &q)

	res := h.do("POST", "/api/quizzes/"+q.ID+"/sessions", nil, nil)
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("publishing a quiz with no questions got %d, want 422", res.StatusCode)
	}
	var errBody map[string]string
	_ = json.NewDecoder(res.Body).Decode(&errBody)
	if !strings.Contains(errBody["error"], "at least one question") {
		t.Errorf("unhelpful error message: %q", errBody["error"])
	}
}

func TestFullPlaythroughOverHTTP(t *testing.T) {
	h := newHarness(t)
	quizID := h.seedQuiz()

	var sess sessionDTO
	res := h.json("POST", "/api/quizzes/"+quizID+"/sessions", nil, nil, &sess)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("publish: status %d", res.StatusCode)
	}
	if len(sess.Code) != 6 {
		t.Fatalf("join code %q should be 6 characters", sess.Code)
	}
	if !strings.HasSuffix(sess.JoinURL, "/j/"+sess.Code) {
		t.Errorf("join URL = %q, want it to end in /j/%s", sess.JoinURL, sess.Code)
	}

	// Two participants join with no auth at all.
	type joinResp struct{ ID, Token, Name, Avatar string }
	var p1, p2 joinResp
	h.json("POST", "/api/sessions/"+sess.Code+"/join", map[string]string{"name": "Ada"}, nil, &p1)
	h.json("POST", "/api/sessions/"+sess.Code+"/join", map[string]string{"name": "Bob"}, nil, &p2)
	if p1.Token == "" || p2.Token == "" || p1.Token == p2.Token {
		t.Fatal("participants must receive distinct tokens")
	}
	if p1.Avatar == "" {
		t.Error("participant should get an avatar")
	}

	// A participant cannot drive the session.
	res = h.do("POST", "/api/sessions/"+sess.Code+"/control",
		map[string]string{"action": "start"}, map[string]string{"X-Host-Token": "not-the-host-aaaaaaaaaaaaaaaaaaa"})
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("non-host control got %d, want 403", res.StatusCode)
	}

	// Organiser starts.
	res = h.do("POST", "/api/sessions/"+sess.Code+"/control", map[string]string{"action": "start"}, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("start: status %d", res.StatusCode)
	}

	// Answering without a player token is refused.
	res = h.do("POST", "/api/sessions/"+sess.Code+"/answer",
		map[string]int{"questionIndex": 0, "optionIndex": 0}, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated answer got %d, want 401", res.StatusCode)
	}

	var ar session.AnswerResult
	h.json("POST", "/api/sessions/"+sess.Code+"/answer",
		map[string]int{"questionIndex": 0, "optionIndex": 0},
		map[string]string{"X-Player-Token": p1.Token}, &ar)
	if !ar.Correct || ar.Points <= 0 {
		t.Errorf("Ada's correct answer: correct=%v points=%d", ar.Correct, ar.Points)
	}
	h.json("POST", "/api/sessions/"+sess.Code+"/answer",
		map[string]int{"questionIndex": 0, "optionIndex": 1},
		map[string]string{"X-Player-Token": p2.Token}, nil)

	// Both answered, so the engine should already be at reveal.
	waitFor(t, func() bool {
		s, _ := h.st.SessionSnapshot(sess.Code)
		return s.Phase == model.PhaseReveal
	}, "reveal after everyone answered")

	h.do("POST", "/api/sessions/"+sess.Code+"/control", map[string]string{"action": "next"}, nil).Body.Close()
	h.json("POST", "/api/sessions/"+sess.Code+"/answer",
		map[string]int{"questionIndex": 1, "optionIndex": 1},
		map[string]string{"X-Player-Token": p1.Token}, nil)
	h.do("POST", "/api/sessions/"+sess.Code+"/control", map[string]string{"action": "end"}, nil).Body.Close()

	// Reports are available immediately and are host-only.
	for _, f := range []string{"pdf", "leaderboard", "questions", "attendees", "responses"} {
		res := h.do("GET", "/api/sessions/"+sess.Code+"/report?format="+f, nil, nil)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("report %s: status %d", f, res.StatusCode)
			continue
		}
		if len(body) < 200 {
			t.Errorf("report %s is suspiciously small (%d bytes)", f, len(body))
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Errorf("report %s missing attachment disposition: %q", f, cd)
		}
		if f == "pdf" && !bytes.HasPrefix(body, []byte("%PDF-")) {
			t.Errorf("pdf report does not start with a PDF header")
		}
		if f != "pdf" && !strings.Contains(string(body), "Ada") {
			t.Errorf("csv report %s does not mention a participant", f)
		}
	}

	res = h.do("GET", "/api/sessions/"+sess.Code+"/report?format=pdf",
		nil, map[string]string{"X-Host-Token": "intruder-token-aaaaaaaaaaaaaaaaaa"})
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("non-host report download got %d, want 403", res.StatusCode)
	}
}

func TestSSEStreamDeliversState(t *testing.T) {
	h := newHarness(t)
	quizID := h.seedQuiz()
	var sess sessionDTO
	h.json("POST", "/api/quizzes/"+quizID+"/sessions", nil, nil, &sess)

	type joinResp struct{ ID, Token string }
	var p joinResp
	h.json("POST", "/api/sessions/"+sess.Code+"/join", map[string]string{"name": "Ada"}, nil, &p)

	req, _ := http.NewRequest("GET",
		h.srv.URL+"/api/sessions/"+sess.Code+"/events?role=participant&token="+p.Token, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}

	frames := make(chan map[string]any, 8)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
				select {
				case frames <- m:
				default:
				}
			}
		}
	}()

	// First frame primes the client with the lobby.
	first := recv(t, frames, "initial state")
	if first["phase"] != "lobby" {
		t.Errorf("initial phase = %v, want lobby", first["phase"])
	}
	if _, ok := first["serverNow"].(float64); !ok {
		t.Error("state frame must carry serverNow for clock-offset correction")
	}
	if _, ok := first["you"]; !ok {
		t.Error("participant frame must carry personal 'you' state")
	}

	// Starting the quiz must push a question within a second.
	h.do("POST", "/api/sessions/"+sess.Code+"/control", map[string]string{"action": "start"}, nil).Body.Close()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-frames:
			if f["phase"] == "question" {
				q, ok := f["question"].(map[string]any)
				if !ok {
					t.Fatal("question frame has no question payload")
				}
				if _, leaked := q["correctIndex"]; leaked {
					t.Error("live question frame leaked the correct answer to a participant")
				}
				if _, has := q["deadlineMs"]; !has {
					t.Error("question frame must carry a server deadline")
				}
				return
			}
		case <-deadline:
			t.Fatal("no question frame arrived within 2s of start")
		}
	}
}

func recv(t *testing.T, ch chan map[string]any, what string) map[string]any {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMediaUploadValidatesAndCompresses(t *testing.T) {
	h := newHarness(t)

	// A large photo-like PNG (smooth gradients, as a camera produces) should be
	// downscaled and re-encoded to something much smaller.
	big := image.NewRGBA(image.Rect(0, 0, 2400, 1600))
	for y := 0; y < 1600; y++ {
		for x := 0; x < 2400; x++ {
			big.Set(x, y, color.RGBA{
				R: uint8(x * 255 / 2400),
				G: uint8(y * 255 / 1600),
				B: uint8((x + y) * 255 / 4000),
				A: 255,
			})
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, big); err != nil {
		t.Fatal(err)
	}

	var m model.Media
	res := h.upload("photo.png", raw.Bytes(), &m)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: status %d", res.StatusCode)
	}
	if m.Kind != model.MediaImage {
		t.Errorf("kind = %q, want image", m.Kind)
	}
	if m.Width > maxDimension || m.Width == 0 {
		t.Errorf("width = %d, want 0 < w <= %d", m.Width, maxDimension)
	}
	if m.Size >= int64(raw.Len()) {
		t.Errorf("stored size %d not smaller than original %d — compression did nothing", m.Size, raw.Len())
	}

	// And it must be served back with a real image content type.
	res2, err := http.Get(h.srv.URL + "/media/" + m.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("serving media: status %d", res2.StatusCode)
	}
	if ct := res2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Errorf("media content type = %q", ct)
	}
	if res2.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("media must advertise range support so mobile Safari can play video")
	}

	// Executables dressed up as images are rejected.
	res3 := h.upload("payload.png", []byte("MZ\x90\x00\x03 this is not an image at all"), nil)
	res3.Body.Close()
	if res3.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("bogus upload got %d, want 415", res3.StatusCode)
	}

	// An empty file is a user mistake, not a media type problem.
	res4 := h.upload("nothing.png", nil, nil)
	res4.Body.Close()
	if res4.StatusCode != http.StatusBadRequest {
		t.Errorf("empty upload got %d, want 400", res4.StatusCode)
	}
}

// TestUploadNeverGrowsAFile guards the case where re-encoding would produce a
// larger file than the original: the upload must be stored as-is.
func TestUploadNeverGrowsAFile(t *testing.T) {
	h := newHarness(t)
	noisy := image.NewRGBA(image.Rect(0, 0, 300, 300))
	for y := 0; y < 300; y++ {
		for x := 0; x < 300; x++ {
			noisy.Set(x, y, color.RGBA{uint8(x * y % 251), uint8((x ^ y) % 253), uint8((x + 3*y) % 247), 255})
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, noisy); err != nil {
		t.Fatal(err)
	}
	var m model.Media
	res := h.upload("noise.png", raw.Bytes(), &m)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload: status %d", res.StatusCode)
	}
	if m.Size > int64(raw.Len()) {
		t.Errorf("stored %d bytes for a %d byte upload — processing made it worse", m.Size, raw.Len())
	}
	if m.Width != 300 || m.Heigh != 300 {
		t.Errorf("dimensions = %dx%d, want 300x300", m.Width, m.Heigh)
	}
}

func (h *harness) upload(name string, data []byte, out any) *http.Response {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()

	req, _ := http.NewRequest("POST", h.srv.URL+"/api/media", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Host-Token", testHostToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if out != nil {
		defer res.Body.Close()
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res
}

func TestQRCodeAndJoinRedirect(t *testing.T) {
	h := newHarness(t)
	quizID := h.seedQuiz()
	var sess sessionDTO
	h.json("POST", "/api/quizzes/"+quizID+"/sessions", nil, nil, &sess)

	res, err := http.Get(h.srv.URL + "/qr/" + sess.Code + ".png")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("qr: status %d", res.StatusCode)
	}
	if res.Header.Get("Content-Type") != "image/png" {
		t.Errorf("qr content type = %q", res.Header.Get("Content-Type"))
	}
	if _, err := png.Decode(bytes.NewReader(body)); err != nil {
		t.Errorf("qr is not a decodable PNG: %v", err)
	}

	// Unknown codes must not produce a QR for a session that does not exist.
	res2, _ := http.Get(h.srv.URL + "/qr/999999.png")
	res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Errorf("qr for unknown session got %d, want 404", res2.StatusCode)
	}

	// The short join link redirects into the app's join route.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res3, err := client.Get(h.srv.URL + "/j/" + sess.Code)
	if err != nil {
		t.Fatal(err)
	}
	res3.Body.Close()
	if res3.StatusCode != http.StatusFound {
		t.Fatalf("join redirect status = %d, want 302", res3.StatusCode)
	}
	if loc := res3.Header.Get("Location"); loc != "/#/join/"+sess.Code {
		t.Errorf("redirect target = %q", loc)
	}
}

func TestSPAFallbackAndSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	res, err := http.Get(h.srv.URL + "/some/deep/client/route")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "KwiqTest") {
		t.Errorf("SPA fallback failed: status %d", res.StatusCode)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("weak or missing CSP: %q", csp)
	}
}

func TestGzipAppliesToAssetsButNotToStreams(t *testing.T) {
	h := newHarness(t)

	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		// Stop the transport from transparently negotiating and stripping it.
		res, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return res
	}

	// The app shell is the payload every participant downloads; it must shrink.
	res := get("/")
	defer res.Body.Close()
	if enc := res.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("index.html Content-Encoding = %q, want gzip", enc)
	}
	if !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") {
		t.Error("a compressed response must Vary on Accept-Encoding")
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	// If a length is advertised it must describe the bytes actually sent, not
	// the pre-compression body — otherwise the client hangs or truncates.
	if cl := res.Header.Get("Content-Length"); cl != "" && cl != fmt.Sprint(len(raw)) {
		t.Errorf("Content-Length = %s but %d compressed bytes were sent", cl, len(raw))
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	body, _ := io.ReadAll(gr)
	if !strings.Contains(string(body), "KwiqTest") {
		t.Error("decompressed shell does not contain the app")
	}
	if len(raw) >= len(body) {
		t.Errorf("gzip did not shrink the shell: %d -> %d bytes", len(body), len(raw))
	}

	// The realtime stream must stay uncompressed: a compressor sitting in that
	// path could hold a question push in its buffer.
	quizID := h.seedQuiz()
	var sess sessionDTO
	h.json("POST", "/api/quizzes/"+quizID+"/sessions", nil, nil, &sess)
	var p struct{ ID, Token string }
	h.json("POST", "/api/sessions/"+sess.Code+"/join", map[string]string{"name": "Ada"}, nil, &p)

	sseRes := get("/api/sessions/" + sess.Code + "/events?role=participant&token=" + p.Token)
	defer sseRes.Body.Close()
	if enc := sseRes.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("SSE stream Content-Encoding = %q, want none", enc)
	}

	// Already-compressed media should not be run through gzip again.
	var m model.Media
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	_ = png.Encode(&buf, img)
	h.upload("tiny.png", buf.Bytes(), &m).Body.Close()

	mediaRes := get("/media/" + m.ID)
	defer mediaRes.Body.Close()
	if enc := mediaRes.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("media Content-Encoding = %q, want none", enc)
	}

	// Tiny replies are sent as-is: gzip framing would make them bigger.
	small := get("/api/health")
	defer small.Body.Close()
	tiny, _ := io.ReadAll(small.Body)
	if enc := small.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("a %d-byte reply was compressed anyway (encoding %q)", len(tiny), enc)
	}
	if !strings.Contains(string(tiny), "ok") {
		t.Errorf("health body = %q", string(tiny))
	}
}

func TestClientsWithoutGzipStillGetTheApp(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("GET", h.srv.URL+"/", nil)
	req.Header.Set("Accept-Encoding", "identity")
	res, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if enc := res.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q for a client that did not ask for gzip", enc)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "KwiqTest") {
		t.Error("uncompressed shell is not served correctly")
	}
}

func TestDeviceDetection(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148": "mobile",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Mobile Safari/537.36":          "mobile",
		"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15":                        "tablet",
		"Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 Safari/537.36":                 "tablet",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120 Safari/537.36":     "desktop",
		"": "unknown",
	}
	for ua, want := range cases {
		if got := deviceOf(ua); got != want {
			t.Errorf("deviceOf(%.40s…) = %q, want %q", ua, got, want)
		}
	}
}
