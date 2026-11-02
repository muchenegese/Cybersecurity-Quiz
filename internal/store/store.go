// Package store is KwiqTest's persistence layer.
//
// There is no database engine. Live state lives in memory (that is what makes
// the realtime path fast) and is mirrored to plain JSON files inside a single
// portable data directory, so the whole app — binary plus data folder — can be
// copied to a USB stick and run anywhere with no install step.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"kwiqtest/internal/model"
)

// ErrNotFound is returned for any missing quiz, session or media asset.
var ErrNotFound = errors.New("not found")

// Store owns all application state.
type Store struct {
	mu       sync.RWMutex
	dir      string
	quizzes  map[string]*model.Quiz
	sessions map[string]*model.Session

	dirtyQuiz    map[string]bool
	dirtySession map[string]bool
	flushCh      chan struct{}
	closeOnce    sync.Once
	done         chan struct{}

	// Retention determines how long a finished session's data is kept.
	Retention time.Duration
}

// Open loads any existing data from dir, creating it if needed.
func Open(dir string, retention time.Duration) (*Store, error) {
	s := &Store{
		dir:          dir,
		quizzes:      map[string]*model.Quiz{},
		sessions:     map[string]*model.Session{},
		dirtyQuiz:    map[string]bool{},
		dirtySession: map[string]bool{},
		flushCh:      make(chan struct{}, 1),
		done:         make(chan struct{}),
		Retention:    retention,
	}
	for _, sub := range []string{"quizzes", "sessions", "media"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	go s.flushLoop()
	return s, nil
}

// Dir reports the data directory in use.
func (s *Store) Dir() string { return s.dir }

type quizFile struct {
	Quiz      *model.Quiz `json:"quiz"`
	HostToken string      `json:"hostToken"`
}

type sessionFile struct {
	Session   *model.Session      `json:"session"`
	HostToken string              `json:"hostToken"`
	Tokens    map[string]string   `json:"tokens"` // participantID -> token
	Answers   []storedAnswer      `json:"answers"`
	Extra     map[string][]string `json:"extra,omitempty"`
}

type storedAnswer struct {
	model.Answer
	AnsweredAt time.Time `json:"answeredAt"`
}

func (s *Store) load() error {
	qs, _ := filepath.Glob(filepath.Join(s.dir, "quizzes", "*.json"))
	for _, p := range qs {
		var qf quizFile
		if err := readJSON(p, &qf); err != nil || qf.Quiz == nil {
			continue
		}
		qf.Quiz.HostToken = qf.HostToken
		s.quizzes[qf.Quiz.ID] = qf.Quiz
	}
	ss, _ := filepath.Glob(filepath.Join(s.dir, "sessions", "*.json"))
	for _, p := range ss {
		var sf sessionFile
		if err := readJSON(p, &sf); err != nil || sf.Session == nil {
			continue
		}
		sess := sf.Session
		sess.HostToken = sf.HostToken
		for _, pt := range sess.Participants {
			pt.Token = sf.Tokens[pt.ID]
			pt.Connected = false // nobody is connected at boot
		}
		sess.Answers = sess.Answers[:0]
		for _, a := range sf.Answers {
			ans := a.Answer
			ans.AnsweredAt = a.AnsweredAt
			sess.Answers = append(sess.Answers, ans)
		}
		// A session that was mid-question when the process stopped cannot
		// resume its timer honestly, so it parks in reveal for the organiser.
		if sess.Phase == model.PhaseQuestion {
			sess.Phase = model.PhaseReveal
			sess.Paused = true
		}
		s.sessions[sess.Code] = sess
	}
	s.purgeExpiredLocked()
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeJSONAtomic writes via a temp file + rename so a crash mid-write can
// never leave a half-written record behind.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ------------------------------------------------------------ flushing ------

func (s *Store) flushLoop() {
	t := time.NewTicker(1500 * time.Millisecond)
	defer t.Stop()
	purge := time.NewTicker(10 * time.Minute)
	defer purge.Stop()
	for {
		select {
		case <-s.done:
			s.Flush()
			return
		case <-s.flushCh:
			s.Flush()
		case <-t.C:
			s.Flush()
		case <-purge.C:
			s.mu.Lock()
			s.purgeExpiredLocked()
			s.mu.Unlock()
		}
	}
}

func (s *Store) markQuiz(id string)   { s.dirtyQuiz[id] = true; s.nudge() }
func (s *Store) markSession(c string) { s.dirtySession[c] = true; s.nudge() }
func (s *Store) nudge() {
	select {
	case s.flushCh <- struct{}{}:
	default:
	}
}

// Flush writes every pending change to disk.
func (s *Store) Flush() {
	s.mu.Lock()
	quizIDs := keys(s.dirtyQuiz)
	sessCodes := keys(s.dirtySession)
	s.dirtyQuiz = map[string]bool{}
	s.dirtySession = map[string]bool{}

	type job struct {
		path string
		val  any
	}
	var jobs []job
	for _, id := range quizIDs {
		q, ok := s.quizzes[id]
		if !ok {
			continue
		}
		cp := *q
		jobs = append(jobs, job{filepath.Join(s.dir, "quizzes", id+".json"),
			quizFile{Quiz: &cp, HostToken: q.HostToken}})
	}
	for _, code := range sessCodes {
		sess, ok := s.sessions[code]
		if !ok {
			continue
		}
		sf := sessionFile{Session: sess, HostToken: sess.HostToken, Tokens: map[string]string{}}
		for _, p := range sess.Participants {
			sf.Tokens[p.ID] = p.Token
		}
		for _, a := range sess.Answers {
			sf.Answers = append(sf.Answers, storedAnswer{Answer: a, AnsweredAt: a.AnsweredAt})
		}
		b, _ := json.Marshal(sf)
		var clone sessionFile
		_ = json.Unmarshal(b, &clone)
		jobs = append(jobs, job{filepath.Join(s.dir, "sessions", code+".json"), clone})
	}
	s.mu.Unlock()

	for _, j := range jobs {
		_ = writeJSONAtomic(j.path, j.val)
	}
}

// Close flushes and stops the background writer.
func (s *Store) Close() {
	s.closeOnce.Do(func() { close(s.done) })
	s.Flush()
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ------------------------------------------------------------ retention -----

func (s *Store) purgeExpiredLocked() {
	now := time.Now()
	for code, sess := range s.sessions {
		if !sess.ExpiresAt.IsZero() && now.After(sess.ExpiresAt) {
			delete(s.sessions, code)
			_ = os.Remove(filepath.Join(s.dir, "sessions", code+".json"))
		}
	}
}

// ---------------------------------------------------------------- ids -------

// codeAlphabet is deliberately alphanumeric rather than digits-only: it raises
// the join-code keyspace from 10^6 (a six-digit PIN, ~20 bits) to 32^6 (~30
// bits — over 1000x harder to guess or brute-force) while staying short enough
// to read off a screen and type on a phone. Visually ambiguous characters —
// 0/O, 1/I/L — are excluded so a code never has to be read aloud or re-typed
// because of a misread glyph.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

func randInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// NewID returns a URL-safe random identifier.
func NewID() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewToken returns a 128-bit secret.
func NewToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newCode returns an unused 6-character alphanumeric join code.
func (s *Store) newCodeLocked() string {
	for attempt := 0; ; attempt++ {
		var sb strings.Builder
		for i := 0; i < 6; i++ {
			sb.WriteByte(codeAlphabet[randInt(len(codeAlphabet))])
		}
		c := sb.String()
		if _, taken := s.sessions[c]; !taken {
			return c
		}
		if attempt > 5000 { // effectively unreachable
			return strings.ToUpper(NewID()[:6])
		}
	}
}

// --------------------------------------------------------------- quizzes ----

// CreateQuiz stores a new quiz and returns it along with its host token.
func (s *Store) CreateQuiz(title, description string) *model.Quiz {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	q := &model.Quiz{
		ID:          NewID(),
		HostToken:   NewToken(),
		Title:       title,
		Description: description,
		Questions:   []model.Question{},
		Settings:    model.DefaultSettings(),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.quizzes[q.ID] = q
	s.markQuiz(q.ID)
	return q
}

// Quiz returns a quiz by id.
func (s *Store) Quiz(id string) (*model.Quiz, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q, ok := s.quizzes[id]
	if !ok {
		return nil, ErrNotFound
	}
	return q, nil
}

// UpdateQuiz applies fn to a quiz under the write lock and persists the result.
func (s *Store) UpdateQuiz(id string, fn func(*model.Quiz) error) (*model.Quiz, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.quizzes[id]
	if !ok {
		return nil, ErrNotFound
	}
	if err := fn(q); err != nil {
		return nil, err
	}
	q.UpdatedAt = time.Now()
	s.markQuiz(id)
	return q, nil
}

// DeleteQuiz removes a quiz and its file.
func (s *Store) DeleteQuiz(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.quizzes[id]; !ok {
		return ErrNotFound
	}
	delete(s.quizzes, id)
	delete(s.dirtyQuiz, id)
	return os.Remove(filepath.Join(s.dir, "quizzes", id+".json"))
}

// QuizzesForHost lists quizzes owned by the given host token, newest first.
func (s *Store) QuizzesForHost(token string) []*model.Quiz {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Quiz
	if token == "" {
		return out
	}
	for _, q := range s.quizzes {
		if q.HostToken == token {
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// -------------------------------------------------------------- sessions ----

// CreateSession opens a live session for a quiz.
func (s *Store) CreateSession(quizID, hostToken string, ttl time.Duration) *model.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	sess := &model.Session{
		Code:         s.newCodeLocked(),
		QuizID:       quizID,
		HostToken:    hostToken,
		CreatedAt:    now,
		ExpiresAt:    now.Add(ttl),
		Phase:        model.PhaseLobby,
		CurrentIndex: -1,
		Participants: []*model.Participant{},
		Answers:      []model.Answer{},
	}
	s.sessions[sess.Code] = sess
	s.markSession(sess.Code)
	return sess
}

// Session returns the live session object.
//
// The returned pointer is shared mutable state owned by the session engine.
// Only fields that never change after creation (Code, QuizID, HostToken) may be
// read from it without holding the store lock. Anything that moves during a
// quiz — phase, participants, answers, timers — must come from SessionSnapshot.
func (s *Store) Session(code string) (*model.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[code]
	if !ok {
		return nil, ErrNotFound
	}
	return sess, nil
}

// SessionSnapshot returns a detached copy safe to read and render from any
// goroutine while a quiz is running. Participants are copied by value, so a
// report or dashboard can never observe a score being updated mid-read.
func (s *Store) SessionSnapshot(code string) (*model.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[code]
	if !ok {
		return nil, ErrNotFound
	}
	return snapshotLocked(sess), nil
}

func snapshotLocked(sess *model.Session) *model.Session {
	cp := *sess
	cp.Participants = make([]*model.Participant, len(sess.Participants))
	for i, p := range sess.Participants {
		q := *p
		cp.Participants[i] = &q
	}
	cp.Answers = append([]model.Answer(nil), sess.Answers...)
	return &cp
}

// SessionsForHost lists snapshots of a host's sessions, newest first.
func (s *Store) SessionsForHost(token string) []*model.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Session
	if token == "" {
		return out
	}
	for _, sess := range s.sessions {
		if sess.HostToken == token {
			out = append(out, snapshotLocked(sess))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// TouchSession marks a session dirty so the next flush persists it.
func (s *Store) TouchSession(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markSession(code)
}

// Lock/Unlock expose the store mutex to the session engine, which needs to make
// multi-field state transitions atomically.
func (s *Store) Lock()    { s.mu.Lock() }
func (s *Store) Unlock()  { s.mu.Unlock() }
func (s *Store) RLock()   { s.mu.RLock() }
func (s *Store) RUnlock() { s.mu.RUnlock() }

// SessionLocked/QuizLocked are lookups for callers already holding the lock.
func (s *Store) SessionLocked(code string) (*model.Session, bool) {
	sess, ok := s.sessions[code]
	return sess, ok
}
func (s *Store) QuizLocked(id string) (*model.Quiz, bool) {
	q, ok := s.quizzes[id]
	return q, ok
}

// MarkSessionLocked flags a session dirty; caller must hold the write lock.
func (s *Store) MarkSessionLocked(code string) { s.markSession(code) }

// AllSessionCodes lists every live session code.
func (s *Store) AllSessionCodes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.AllSessionCodesLocked()
}

// AllSessionCodesLocked is AllSessionCodes for callers already holding a lock.
func (s *Store) AllSessionCodesLocked() []string {
	out := make([]string, 0, len(s.sessions))
	for c := range s.sessions {
		out = append(out, c)
	}
	return out
}

// ----------------------------------------------------------------- media ----

// MediaPath returns the on-disk path for a stored asset id.
func (s *Store) MediaPath(id string) string {
	return filepath.Join(s.dir, "media", filepath.Base(id))
}

// SaveMedia writes bytes for a new asset and returns its id (name + extension).
func (s *Store) SaveMedia(ext string, data []byte) (string, error) {
	id := NewID() + ext
	if err := os.WriteFile(s.MediaPath(id), data, 0o644); err != nil {
		return "", err
	}
	return id, nil
}
