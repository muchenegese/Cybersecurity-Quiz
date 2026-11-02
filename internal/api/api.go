// Package api exposes the HTTP surface: a small JSON API, an SSE stream, the
// media store, QR codes, report downloads, and the embedded single-page app.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kwiqtest/internal/hub"
	"kwiqtest/internal/model"
	"kwiqtest/internal/session"
	"kwiqtest/internal/store"
)

// Config tunes the server's limits and lifetimes.
type Config struct {
	MaxImageBytes int64
	MaxVideoBytes int64
	SessionTTL    time.Duration
	PublicBase    string // e.g. "http://192.168.1.20:37390", used for join links/QR
}

// DefaultConfig mirrors the limits recommended by the PRD.
func DefaultConfig() Config {
	return Config{
		MaxImageBytes: 5 << 20,  // 5 MB
		MaxVideoBytes: 50 << 20, // 50 MB
		SessionTTL:    24 * time.Hour,
	}
}

// Server bundles the dependencies every handler needs.
type Server struct {
	st  *store.Store
	hb  *hub.Hub
	eng *session.Engine
	web fs.FS
	cfg Config
}

// New builds a Server.
func New(st *store.Store, hb *hub.Hub, eng *session.Engine, web fs.FS, cfg Config) *Server {
	return &Server{st: st, hb: hb, eng: eng, web: web, cfg: cfg}
}

// Routes returns the fully wired handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Quizzes (organiser, guest or otherwise — identity is just a bearer token
	// the client generates and keeps; no signup required to host).
	mux.HandleFunc("GET /api/quizzes", s.listQuizzes)
	mux.HandleFunc("POST /api/quizzes", s.createQuiz)
	mux.HandleFunc("GET /api/quizzes/{id}", s.getQuiz)
	mux.HandleFunc("PUT /api/quizzes/{id}", s.updateQuiz)
	mux.HandleFunc("DELETE /api/quizzes/{id}", s.deleteQuiz)
	mux.HandleFunc("POST /api/quizzes/{id}/duplicate", s.duplicateQuiz)
	mux.HandleFunc("POST /api/quizzes/{id}/sessions", s.publishSession)

	// Media: local-drive uploads only. There is deliberately no endpoint that
	// ingests a remote URL.
	mux.HandleFunc("POST /api/media", s.uploadMedia)
	mux.HandleFunc("GET /media/{id}", s.serveMedia)

	// Sessions
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("GET /api/sessions/{code}", s.sessionInfo)
	mux.HandleFunc("POST /api/sessions/{code}/join", s.join)
	mux.HandleFunc("GET /api/sessions/{code}/events", s.events)
	mux.HandleFunc("POST /api/sessions/{code}/answer", s.answer)
	mux.HandleFunc("POST /api/sessions/{code}/control", s.control)
	mux.HandleFunc("GET /api/sessions/{code}/report", s.report)
	mux.HandleFunc("GET /api/sessions/{code}/analytics", s.analytics)

	mux.HandleFunc("GET /qr/{code}", s.qr)
	mux.HandleFunc("GET /j/{code}", s.joinRedirect)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now()})
	})

	mux.Handle("/", s.spa())
	return securityHeaders(withGzip(mux))
}

// ------------------------------------------------------------- helpers ------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, session.ErrLocked), errors.Is(err, session.ErrEnded):
		return http.StatusConflict
	case errors.Is(err, session.ErrRateLimited):
		return http.StatusTooManyRequests
	default:
		return http.StatusBadRequest
	}
}

func hostToken(r *http.Request) string {
	if t := r.Header.Get("X-Host-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("host")
}

func playerToken(r *http.Request) string {
	if t := r.Header.Get("X-Player-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		return strings.TrimSpace(strings.Split(h, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// deviceOf classifies the joining device for the attendee report.
func deviceOf(ua string) string {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet") ||
		(strings.Contains(l, "android") && !strings.Contains(l, "mobile")):
		return "tablet"
	case strings.Contains(l, "mobi") || strings.Contains(l, "iphone") ||
		strings.Contains(l, "android"):
		return "mobile"
	case l == "":
		return "unknown"
	default:
		return "desktop"
	}
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	return dec.Decode(v)
}

// codeParam reads the {code} path segment and normalises it to the case join
// codes are always stored/generated in, so a code typed, pasted or scanned in
// any case still resolves — the alphabet is uppercase-only, but users on a
// mobile keyboard should not have to fight autocapitalisation either way.
func codeParam(r *http.Request) string {
	return strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
}

// securityHeaders applies a conservative baseline to every response. The CSP is
// strict — the app ships no third-party scripts, fonts or trackers at all.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; "+
				"style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; "+
				"frame-ancestors 'self'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// baseURL determines the externally reachable origin for join links and QR
// codes, preferring an explicitly configured value over the request's own host.
//
// Behind a reverse proxy the request's Host is frequently the upstream address
// (nginx's proxy_pass rewrites it to 127.0.0.1:port by default), which would
// bake an unreachable URL into every QR code. X-Forwarded-Host carries the
// original name in that case. It is attacker-controllable when no proxy strips
// it, so -public remains the authoritative setting for a public deployment.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicBase != "" {
		return strings.TrimRight(s.cfg.PublicBase, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		// A chain of proxies appends; the first entry is the client-facing one.
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = fwd[:i]
		}
		if fwd = strings.TrimSpace(fwd); fwd != "" {
			host = fwd
		}
	}
	return scheme + "://" + host
}

// -------------------------------------------------------------- quizzes -----

type quizDTO struct {
	*model.Quiz
	QuestionCount int `json:"questionCount"`
}

func (s *Server) listQuizzes(w http.ResponseWriter, r *http.Request) {
	token := hostToken(r)
	if token == "" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	list := s.st.QuizzesForHost(token)
	out := make([]quizDTO, 0, len(list))
	for _, q := range list {
		out = append(out, quizDTO{Quiz: q, QuestionCount: len(q.Questions)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createQuiz(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title       string           `json:"title"`
		Description string           `json:"description"`
		Questions   []model.Question `json:"questions"`
		Settings    *model.Settings  `json:"settings"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	token := hostToken(r)
	if len(token) < 16 {
		writeErr(w, http.StatusBadRequest, "missing or weak host token")
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		body.Title = "Untitled quiz"
	}
	q := s.st.CreateQuiz(body.Title, body.Description)
	_, _ = s.st.UpdateQuiz(q.ID, func(q *model.Quiz) error {
		q.HostToken = token
		if body.Questions != nil {
			q.Questions = normaliseQuestions(body.Questions)
		}
		if body.Settings != nil {
			q.Settings = *body.Settings
		}
		return nil
	})
	writeJSON(w, http.StatusCreated, q)
}

// ownedQuiz fetches a quiz and verifies the caller owns it.
func (s *Server) ownedQuiz(r *http.Request) (*model.Quiz, error) {
	q, err := s.st.Quiz(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if q.HostToken != hostToken(r) {
		return nil, store.ErrNotFound // do not confirm existence to non-owners
	}
	return q, nil
}

func (s *Server) getQuiz(w http.ResponseWriter, r *http.Request) {
	q, err := s.ownedQuiz(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, "quiz not found")
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) updateQuiz(w http.ResponseWriter, r *http.Request) {
	if _, err := s.ownedQuiz(r); err != nil {
		writeErr(w, http.StatusNotFound, "quiz not found")
		return
	}
	var body struct {
		Title       *string          `json:"title"`
		Description *string          `json:"description"`
		Cover       *model.Media     `json:"cover"`
		Questions   []model.Question `json:"questions"`
		Settings    *model.Settings  `json:"settings"`
		IsTemplate  *bool            `json:"isTemplate"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	q, err := s.st.UpdateQuiz(r.PathValue("id"), func(q *model.Quiz) error {
		if body.Title != nil {
			q.Title = strings.TrimSpace(*body.Title)
		}
		if body.Description != nil {
			q.Description = *body.Description
		}
		if body.Cover != nil {
			if body.Cover.ID == "" {
				q.Cover = nil
			} else {
				q.Cover = body.Cover
			}
		}
		if body.Questions != nil {
			q.Questions = normaliseQuestions(body.Questions)
		}
		if body.Settings != nil {
			q.Settings = *body.Settings
		}
		if body.IsTemplate != nil {
			q.IsTemplate = *body.IsTemplate
		}
		return nil
	})
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, q)
}

// normaliseQuestions clamps incoming values into legal ranges and assigns ids,
// so a hand-crafted request cannot produce a 0-second or 9999-point question.
func normaliseQuestions(in []model.Question) []model.Question {
	out := make([]model.Question, 0, len(in))
	for _, q := range in {
		if q.ID == "" {
			q.ID = store.NewID()
		}
		if q.TimeLimitSec < 5 {
			q.TimeLimitSec = 5
		}
		if q.TimeLimitSec > 120 {
			q.TimeLimitSec = 120
		}
		if q.Points <= 0 {
			q.Points = 1000
		}
		if q.Points > 10000 {
			q.Points = 10000
		}
		if len(q.Options) > 6 {
			q.Options = q.Options[:6]
		}
		if q.CorrectIndex < 0 {
			q.CorrectIndex = 0
		}
		if q.CorrectIndex >= len(q.Options) && len(q.Options) > 0 {
			q.CorrectIndex = len(q.Options) - 1
		}
		out = append(out, q)
	}
	return out
}

func (s *Server) deleteQuiz(w http.ResponseWriter, r *http.Request) {
	if _, err := s.ownedQuiz(r); err != nil {
		writeErr(w, http.StatusNotFound, "quiz not found")
		return
	}
	if err := s.st.DeleteQuiz(r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) duplicateQuiz(w http.ResponseWriter, r *http.Request) {
	src, err := s.ownedQuiz(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, "quiz not found")
		return
	}
	cp := s.st.CreateQuiz(src.Title+" (copy)", src.Description)
	questions := make([]model.Question, len(src.Questions))
	copy(questions, src.Questions)
	for i := range questions {
		questions[i].ID = store.NewID()
	}
	q, _ := s.st.UpdateQuiz(cp.ID, func(q *model.Quiz) error {
		q.HostToken = src.HostToken
		q.Cover = src.Cover
		q.Questions = questions
		q.Settings = src.Settings
		return nil
	})
	writeJSON(w, http.StatusCreated, q)
}

// ------------------------------------------------------------- sessions -----

type sessionDTO struct {
	Code          string    `json:"code"`
	QuizID        string    `json:"quizId"`
	Title         string    `json:"title"`
	Phase         string    `json:"phase"`
	JoinURL       string    `json:"joinUrl"`
	QRURL         string    `json:"qrUrl"`
	CreatedAt     time.Time `json:"createdAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Count         int       `json:"participantCount"`
	Locked        bool      `json:"locked"`
	Total         int       `json:"totalQuestions"`
	AvgScore      int       `json:"avgScore"`
	AvgAccuracyPc int       `json:"avgAccuracyPc"`
	TopScore      int       `json:"topScore"`
}

func (s *Server) dtoFor(r *http.Request, sess *model.Session) sessionDTO {
	title, total := "", 0
	if q, err := s.st.Quiz(sess.QuizID); err == nil {
		title, total = q.Title, len(q.Questions)
	}
	standings := session.Standings(sess)
	avgScore, avgAccuracy, topScore := 0, 0, 0
	if len(standings) > 0 {
		var sumScore, sumAccuracy int
		for _, st := range standings {
			sumScore += st.Score
			sumAccuracy += st.AccuracyPc
		}
		avgScore = sumScore / len(standings)
		avgAccuracy = sumAccuracy / len(standings)
		topScore = standings[0].Score
	}
	return sessionDTO{
		Code: sess.Code, QuizID: sess.QuizID, Title: title, Phase: string(sess.Phase),
		JoinURL:   s.baseURL(r) + "/j/" + sess.Code,
		QRURL:     "/qr/" + sess.Code + ".png",
		CreatedAt: sess.CreatedAt, ExpiresAt: sess.ExpiresAt,
		Count: len(sess.Participants), Locked: sess.Locked, Total: total,
		AvgScore: avgScore, AvgAccuracyPc: avgAccuracy, TopScore: topScore,
	}
}

func (s *Server) publishSession(w http.ResponseWriter, r *http.Request) {
	q, err := s.ownedQuiz(r)
	if err != nil {
		writeErr(w, http.StatusNotFound, "quiz not found")
		return
	}
	if err := q.Validate(); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	created := s.st.CreateSession(q.ID, q.HostToken, s.cfg.SessionTTL)
	sess, err := s.st.SessionSnapshot(created.Code)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not open the session")
		return
	}
	writeJSON(w, http.StatusCreated, s.dtoFor(r, sess))
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	list := s.st.SessionsForHost(hostToken(r))
	quizID := r.URL.Query().Get("quizId")
	out := make([]sessionDTO, 0, len(list))
	for _, sess := range list {
		if quizID != "" && sess.QuizID != quizID {
			continue
		}
		out = append(out, s.dtoFor(r, sess))
	}
	writeJSON(w, http.StatusOK, out)
}

// sessionInfo is the public pre-join lookup: enough to render the join screen,
// nothing that could leak answers.
func (s *Server) sessionInfo(w http.ResponseWriter, r *http.Request) {
	sess, err := s.st.SessionSnapshot(codeParam(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, "session not found — check the code")
		return
	}
	title, cover := "", ""
	if q, err := s.st.Quiz(sess.QuizID); err == nil {
		title = q.Title
		cover = q.Cover.URL()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": sess.Code, "title": title, "cover": cover,
		"phase": sess.Phase, "locked": sess.Locked,
		"participantCount": len(sess.Participants),
		"joinUrl":          s.baseURL(r) + "/j/" + sess.Code,
	})
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	code := codeParam(r)
	p, err := s.eng.Join(code, body.Name, body.Token, deviceOf(r.UserAgent()), clientIP(r))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": p.ID, "token": p.Token, "name": p.Name, "avatar": p.Avatar, "score": p.Score,
	})
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		QuestionIndex int `json:"questionIndex"`
		OptionIndex   int `json:"optionIndex"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	code := codeParam(r)
	p := s.eng.ParticipantByToken(code, playerToken(r))
	if p == nil {
		writeErr(w, http.StatusUnauthorized, "not joined to this session")
		return
	}
	res, err := s.eng.Answer(code, p.ID, body.QuestionIndex, body.OptionIndex)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	code := codeParam(r)
	sess, err := s.st.Session(code)
	if err != nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if sess.HostToken != hostToken(r) {
		writeErr(w, http.StatusForbidden, "only the organiser can control this session")
		return
	}
	var body struct {
		Action        string `json:"action"`
		ParticipantID string `json:"participantId"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.eng.Control(code, body.Action, body.ParticipantID); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ------------------------------------------------------------------ SSE -----

// events is the realtime stream. One HTTP response stays open per client; the
// server writes a complete state snapshot on every change, so a client that
// misses a frame self-heals on the next one.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	code := codeParam(r)
	sess, err := s.st.Session(code)
	if err != nil {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}

	role := hub.RoleParticipant
	participantID := ""
	switch r.URL.Query().Get("role") {
	case "host":
		if sess.HostToken != hostToken(r) {
			writeErr(w, http.StatusForbidden, "not the organiser")
			return
		}
		role = hub.RoleHost
	case "screen":
		role = hub.RoleScreen
	default:
		p := s.eng.ParticipantByToken(code, playerToken(r))
		if p == nil {
			writeErr(w, http.StatusUnauthorized, "not joined to this session")
			return
		}
		participantID = p.ID
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // defeat proxy buffering
	w.WriteHeader(http.StatusOK)
	// Reconnection delay hint for the browser's built-in EventSource retry.
	fmt.Fprint(w, "retry: 1500\n\n")
	flusher.Flush()

	sub := s.hb.Subscribe(code, role, participantID)
	defer s.hb.Unsubscribe(code, sub)

	if frame := s.eng.FrameFor(code, role, participantID); frame != nil {
		_, _ = w.Write(frame)
		flusher.Flush()
	}

	// Comment-only keepalives stop intermediaries (and mobile radios) from
	// treating an idle lobby as a dead connection.
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			return
		case frame := <-sub.C():
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprintf(w, ": ping %d\n\n", time.Now().UnixMilli()); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------- QR & routing ----

func (s *Server) joinRedirect(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSuffix(r.PathValue("code"), "/"))
	http.Redirect(w, r, "/#/join/"+code, http.StatusFound)
}

func (s *Server) qr(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSuffix(r.PathValue("code"), ".png"))
	if _, err := s.st.Session(code); err != nil {
		http.NotFound(w, r)
		return
	}
	size := 512
	if v, err := strconv.Atoi(r.URL.Query().Get("size")); err == nil && v >= 128 && v <= 2048 {
		size = v
	}
	png, err := qrPNG(s.baseURL(r)+"/j/"+code, size)
	if err != nil {
		http.Error(w, "qr generation failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Disposition", `inline; filename="kwiqtest-`+code+`.png"`)
	_, _ = w.Write(png)
}

// ------------------------------------------------------------------ SPA -----

// spa serves the embedded frontend, falling back to index.html so client-side
// routes survive a hard refresh.
func (s *Server) spa() http.Handler {
	fileServer := http.FileServer(http.FS(s.web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := s.web.Open(p); err == nil {
			if st, err := f.Stat(); err == nil && !st.IsDir() {
				_ = f.Close()
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=3600")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
			_ = f.Close()
		}
		index, err := fs.ReadFile(s.web, "index.html")
		if err != nil {
			http.Error(w, "frontend assets missing", http.StatusInternalServerError)
			log.Printf("spa: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
