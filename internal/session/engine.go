// Package session owns the live-quiz state machine.
//
// The server is the single source of truth for phase, timer and score. Clients
// render a countdown against a server-issued deadline corrected by a measured
// clock offset, so nothing can be gained by changing a device's clock.
package session

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"kwiqtest/internal/hub"
	"kwiqtest/internal/model"
	"kwiqtest/internal/store"
)

// Errors returned to API callers.
var (
	ErrNoQuestions = errors.New("quiz has no questions")
	ErrLocked      = errors.New("this session is locked and not accepting new participants")
	ErrEnded       = errors.New("this session has ended")
	ErrBadPhase    = errors.New("action not valid in the current phase")
	ErrNameTaken   = errors.New("name required")
	ErrRateLimited = errors.New("too many join attempts from this device, try again shortly")
)

const (
	tickInterval = 200 * time.Millisecond
	// Grace window added to the client-visible deadline before the server
	// hard-closes a question, absorbing network latency so a participant who
	// tapped at 00:00 on their screen is not unfairly rejected.
	answerGraceMs = 750
)

var avatars = []string{
	"🦊", "🐼", "🐨", "🦁", "🐯", "🦄", "🐸", "🐙", "🦉", "🐳", "🦜", "🐝",
	"🦖", "🐢", "🦔", "🐹", "🦩", "🐬", "🦋", "🐰", "🐺", "🦈", "🐧", "🦥",
}

// Engine coordinates every live session in the process.
type Engine struct {
	st        *store.Store
	hb        *hub.Hub
	mu        sync.Mutex // guards dirty sets below
	dirtyHost map[string]bool
	dirtyAll  map[string]bool

	joinLimiter *rateLimiter
	stop        chan struct{}
	stopOnce    sync.Once
}

// New wires an engine to a store and hub and starts the timer loop.
func New(st *store.Store, hb *hub.Hub) *Engine {
	e := &Engine{
		st:          st,
		hb:          hb,
		dirtyHost:   map[string]bool{},
		dirtyAll:    map[string]bool{},
		joinLimiter: newRateLimiter(40, time.Minute),
		stop:        make(chan struct{}),
	}
	hb.OnChange = func(code string) { e.syncPresence(code); e.markHost(code) }
	go e.loop()
	return e
}

// Close stops the timer loop.
func (e *Engine) Close() { e.stopOnce.Do(func() { close(e.stop) }) }

func nowMs() int64 { return time.Now().UnixMilli() }

// ------------------------------------------------------------- join ---------

// Join adds (or reconnects) a participant. Passing a known token resumes that
// participant with their score intact, which is how a phone that dropped off
// Wi-Fi mid-quiz gets back in without losing points.
func (e *Engine) Join(code, name, token, device, ip string) (*model.Participant, error) {
	e.st.Lock()
	sess, ok := e.st.SessionLocked(code)
	if !ok {
		e.st.Unlock()
		return nil, store.ErrNotFound
	}
	if sess.Phase == model.PhaseEnded {
		e.st.Unlock()
		return nil, ErrEnded
	}

	// Reconnect path: an existing token always wins, even on a locked room.
	if token != "" {
		for _, p := range sess.Participants {
			if p.Token == token {
				p.LastSeenAt = time.Now()
				if n := sanitiseName(name); n != "" && n != p.Name {
					p.Name = uniqueName(sess, n, p.ID)
				}
				e.st.MarkSessionLocked(code)
				e.st.Unlock()
				e.markAll(code)
				return p, nil
			}
		}
	}

	if sess.Locked {
		e.st.Unlock()
		return nil, ErrLocked
	}
	if !e.joinLimiter.allow(ip) {
		e.st.Unlock()
		return nil, ErrRateLimited
	}
	clean := sanitiseName(name)
	if clean == "" {
		e.st.Unlock()
		return nil, ErrNameTaken
	}

	p := &model.Participant{
		ID:         store.NewID(),
		Token:      store.NewToken(),
		Name:       uniqueName(sess, clean, ""),
		Avatar:     avatars[len(sess.Participants)%len(avatars)],
		JoinedAt:   time.Now(),
		LastSeenAt: time.Now(),
		Device:     device,
	}
	// No participant cap: the slice simply grows with however many people join.
	sess.Participants = append(sess.Participants, p)
	e.st.MarkSessionLocked(code)
	e.st.Unlock()

	e.markAll(code)
	return p, nil
}

func sanitiseName(n string) string {
	n = strings.TrimSpace(n)
	n = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, n)
	if len([]rune(n)) > 24 {
		n = string([]rune(n)[:24])
	}
	return n
}

// uniqueName appends " (2)", " (3)" … when a display name is already in use.
func uniqueName(sess *model.Session, name, skipID string) string {
	taken := func(candidate string) bool {
		for _, p := range sess.Participants {
			if p.ID != skipID && strings.EqualFold(p.Name, candidate) {
				return true
			}
		}
		return false
	}
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%d)", name, i)
		if !taken(c) {
			return c
		}
	}
}

// ParticipantByToken resolves a rejoin token within a session.
func (e *Engine) ParticipantByToken(code, token string) *model.Participant {
	e.st.RLock()
	defer e.st.RUnlock()
	sess, ok := e.st.SessionLocked(code)
	if !ok || token == "" {
		return nil
	}
	for _, p := range sess.Participants {
		if p.Token == token {
			return p
		}
	}
	return nil
}

// ------------------------------------------------------------ answers -------

// AnswerResult is what a participant learns immediately after answering.
type AnswerResult struct {
	Accepted bool `json:"accepted"`
	Correct  bool `json:"correct"`
	Points   int  `json:"points"`
	Score    int  `json:"score"`
	Rank     int  `json:"rank"`
}

// Answer records a participant's choice. Answers lock immediately: a second
// submission for the same question is rejected.
func (e *Engine) Answer(code, participantID string, questionIndex, optionIndex int) (*AnswerResult, error) {
	e.st.Lock()
	sess, ok := e.st.SessionLocked(code)
	if !ok {
		e.st.Unlock()
		return nil, store.ErrNotFound
	}
	quiz, ok := e.st.QuizLocked(sess.QuizID)
	if !ok {
		e.st.Unlock()
		return nil, store.ErrNotFound
	}
	if sess.Phase != model.PhaseQuestion || questionIndex != sess.CurrentIndex {
		e.st.Unlock()
		return nil, ErrBadPhase
	}
	if sess.Paused {
		e.st.Unlock()
		return nil, ErrBadPhase
	}
	now := nowMs()
	if now > sess.DeadlineMs+answerGraceMs {
		e.st.Unlock()
		return nil, ErrBadPhase
	}
	q := quiz.Questions[questionIndex]
	if optionIndex < 0 || optionIndex >= len(q.Options) {
		e.st.Unlock()
		return nil, errors.New("invalid option")
	}
	var me *model.Participant
	for _, p := range sess.Participants {
		if p.ID == participantID {
			me = p
			break
		}
	}
	if me == nil {
		e.st.Unlock()
		return nil, store.ErrNotFound
	}
	for _, a := range sess.Answers {
		if a.ParticipantID == participantID && a.QuestionIndex == questionIndex {
			e.st.Unlock()
			return nil, errors.New("answer already locked in")
		}
	}

	elapsed := now - sess.QuestionStartMs
	if elapsed < 0 {
		elapsed = 0
	}
	correct := optionIndex == q.CorrectIndex
	pts := scoreFor(q, quiz.Settings.SpeedBonus, correct, elapsed)

	sess.Answers = append(sess.Answers, model.Answer{
		ParticipantID: participantID,
		QuestionIndex: questionIndex,
		OptionIndex:   optionIndex,
		Correct:       correct,
		Points:        pts,
		ElapsedMs:     elapsed,
		AnsweredAt:    time.Now(),
	})
	me.Score += pts
	me.Answered++
	me.TotalMs += elapsed
	if correct {
		me.Correct++
	}
	rank := rankOf(sess, participantID)
	everyoneAnswered := countAnswers(sess, questionIndex) >= len(sess.Participants) && len(sess.Participants) > 0
	e.st.MarkSessionLocked(code)
	e.st.Unlock()

	// When the last outstanding answer lands there is nothing left to wait for,
	// so close the question early instead of burning the remaining timer.
	if everyoneAnswered {
		_ = e.Control(code, "reveal")
	} else {
		// The organiser's tally updates on the next tick; the person who just
		// answered gets their locked-in state straight away.
		e.markHost(code)
		e.PublishParticipant(code, participantID)
	}
	return &AnswerResult{Accepted: true, Correct: correct, Points: pts, Score: me.Score, Rank: rank}, nil
}

// scoreFor implements the two scoring modes: flat points, or a speed bonus in
// which half the points are guaranteed for a correct answer and half decay
// linearly across the question's time limit.
func scoreFor(q model.Question, speedBonus, correct bool, elapsedMs int64) int {
	if !correct {
		return 0
	}
	if !speedBonus {
		return q.Points
	}
	limit := float64(q.TimeLimitSec) * 1000
	if limit <= 0 {
		return q.Points
	}
	remaining := 1 - float64(elapsedMs)/limit
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	return int(float64(q.Points)*0.5 + float64(q.Points)*0.5*remaining + 0.5)
}

func countAnswers(sess *model.Session, idx int) int {
	n := 0
	for _, a := range sess.Answers {
		if a.QuestionIndex == idx {
			n++
		}
	}
	return n
}

// --------------------------------------------------------- host controls ----

// Control applies an organiser action to the session state machine.
func (e *Engine) Control(code, action string, args ...string) error {
	e.st.Lock()
	sess, ok := e.st.SessionLocked(code)
	if !ok {
		e.st.Unlock()
		return store.ErrNotFound
	}
	quiz, ok := e.st.QuizLocked(sess.QuizID)
	if !ok {
		e.st.Unlock()
		return store.ErrNotFound
	}
	total := len(quiz.Questions)
	if total == 0 {
		e.st.Unlock()
		return ErrNoQuestions
	}
	var err error

	startQuestion := func(idx int) {
		q := quiz.Questions[idx]
		sess.CurrentIndex = idx
		sess.Phase = model.PhaseQuestion
		start := nowMs()
		sess.QuestionStartMs = start
		sess.DeadlineMs = start + int64(q.TimeLimitSec)*1000
		sess.Paused = false
		sess.PausedRemainMs = 0
		// "Start timer after video" is implemented as a held timer rather than
		// a client-side delay: every device finishes buffering at a different
		// moment, so only the organiser can say when the room is ready. The
		// question and its media are pushed immediately; the clock waits.
		if q.TimerAfterVid && q.Media != nil && q.Media.Kind == model.MediaVideo {
			sess.Paused = true
			sess.PausedRemainMs = int64(q.TimeLimitSec) * 1000
		}
		if sess.StartedAt.IsZero() {
			sess.StartedAt = time.Now()
		}
	}
	clearAnswers := func(idx int) {
		kept := sess.Answers[:0]
		for _, a := range sess.Answers {
			if a.QuestionIndex == idx {
				for _, p := range sess.Participants {
					if p.ID == a.ParticipantID {
						p.Score -= a.Points
						p.Answered--
						p.TotalMs -= a.ElapsedMs
						if a.Correct {
							p.Correct--
						}
					}
				}
				continue
			}
			kept = append(kept, a)
		}
		sess.Answers = kept
	}

	switch action {
	case "start":
		if sess.Phase != model.PhaseLobby {
			err = ErrBadPhase
			break
		}
		startQuestion(0)

	case "reveal": // timer expired, question skipped, or everyone answered
		if sess.Phase != model.PhaseQuestion {
			err = ErrBadPhase
			break
		}
		sess.Phase = model.PhaseReveal
		sess.Paused = false
		sess.QuestionStartMs = nowMs() // reveal-screen dwell clock

	case "next":
		switch sess.Phase {
		case model.PhaseQuestion:
			sess.Phase = model.PhaseReveal
			sess.QuestionStartMs = nowMs()
		case model.PhaseReveal:
			if sess.CurrentIndex+1 >= total {
				sess.Phase = model.PhaseEnded
				sess.EndedAt = time.Now()
			} else {
				startQuestion(sess.CurrentIndex + 1)
			}
		case model.PhaseLobby:
			startQuestion(0)
		default:
			err = ErrBadPhase
		}

	case "skip": // abandon the question without scoring the rest of the timer
		if sess.Phase != model.PhaseQuestion {
			err = ErrBadPhase
			break
		}
		sess.Phase = model.PhaseReveal
		sess.QuestionStartMs = nowMs()

	case "repeat": // re-run the current question from scratch
		if sess.CurrentIndex < 0 {
			err = ErrBadPhase
			break
		}
		clearAnswers(sess.CurrentIndex)
		startQuestion(sess.CurrentIndex)

	case "pause":
		if sess.Phase != model.PhaseQuestion || sess.Paused {
			err = ErrBadPhase
			break
		}
		sess.Paused = true
		sess.PausedRemainMs = sess.DeadlineMs - nowMs()
		if sess.PausedRemainMs < 0 {
			sess.PausedRemainMs = 0
		}

	case "resume":
		if sess.Phase != model.PhaseQuestion || !sess.Paused {
			err = ErrBadPhase
			break
		}
		// Shift both ends of the window so elapsed-time scoring stays fair.
		shift := nowMs() + sess.PausedRemainMs - sess.DeadlineMs
		sess.DeadlineMs += shift
		sess.QuestionStartMs += shift
		sess.Paused = false
		sess.PausedRemainMs = 0

	case "end":
		sess.Phase = model.PhaseEnded
		sess.EndedAt = time.Now()
		sess.Paused = false

	case "lock":
		sess.Locked = true
	case "unlock":
		sess.Locked = false

	case "kick":
		if len(args) == 0 {
			err = errors.New("missing participant id")
			break
		}
		id := args[0]
		kept := sess.Participants[:0]
		for _, p := range sess.Participants {
			if p.ID != id {
				kept = append(kept, p)
			}
		}
		sess.Participants = kept

	default:
		err = fmt.Errorf("unknown action %q", action)
	}

	if err == nil {
		e.st.MarkSessionLocked(code)
	}
	e.st.Unlock()
	if err == nil {
		e.markAll(code)
	}
	return err
}

// ---------------------------------------------------------- timer loop ------

func (e *Engine) loop() {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.tickSessions()
			e.flushDirty()
		}
	}
}

// tickSessions advances any session whose deadline has passed.
func (e *Engine) tickSessions() {
	now := nowMs()
	type action struct{ code, act string }
	var todo []action

	e.st.RLock()
	for _, code := range e.st.AllSessionCodesLocked() {
		sess, ok := e.st.SessionLocked(code)
		if !ok || sess.Paused {
			continue
		}
		quiz, ok := e.st.QuizLocked(sess.QuizID)
		if !ok {
			continue
		}
		switch sess.Phase {
		case model.PhaseQuestion:
			if now > sess.DeadlineMs+answerGraceMs {
				todo = append(todo, action{code, "reveal"})
			}
		case model.PhaseReveal:
			if quiz.Settings.AutoAdvance {
				dwell := int64(quiz.Settings.RevealDurationMs)
				if dwell <= 0 {
					dwell = 5000
				}
				if now-sess.QuestionStartMs > dwell {
					todo = append(todo, action{code, "next"})
				}
			}
		}
	}
	e.st.RUnlock()

	for _, a := range todo {
		_ = e.Control(a.code, a.act)
	}
}

// syncPresence reconciles the participant "connected" flags with the hub.
func (e *Engine) syncPresence(code string) {
	_, _, connected := e.hb.Presence(code)
	e.st.Lock()
	if sess, ok := e.st.SessionLocked(code); ok {
		for _, p := range sess.Participants {
			was := p.Connected
			p.Connected = connected[p.ID]
			if p.Connected && !was {
				p.LastSeenAt = time.Now()
			}
		}
	}
	e.st.Unlock()
}

func (e *Engine) markHost(code string) {
	e.mu.Lock()
	e.dirtyHost[code] = true
	e.mu.Unlock()
}

func (e *Engine) markAll(code string) {
	e.mu.Lock()
	e.dirtyAll[code] = true
	e.mu.Unlock()
}

// flushDirty coalesces state changes into at most one broadcast per tick, so a
// burst of 500 simultaneous answers produces five updates a second rather than
// five hundred.
func (e *Engine) flushDirty() {
	e.mu.Lock()
	all := keysOf(e.dirtyAll)
	hostOnly := []string{}
	for c := range e.dirtyHost {
		if !e.dirtyAll[c] {
			hostOnly = append(hostOnly, c)
		}
	}
	e.dirtyAll = map[string]bool{}
	e.dirtyHost = map[string]bool{}
	e.mu.Unlock()

	for _, c := range all {
		e.Publish(c, false)
	}
	for _, c := range hostOnly {
		e.Publish(c, true)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
