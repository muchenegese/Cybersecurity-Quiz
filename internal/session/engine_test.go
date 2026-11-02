package session

import (
	"testing"
	"time"

	"kwiqtest/internal/hub"
	"kwiqtest/internal/model"
	"kwiqtest/internal/store"
)

// newTestEngine spins up a real store in a temp dir plus a hub and engine.
func newTestEngine(t *testing.T) (*Engine, *store.Store, *model.Quiz) {
	t.Helper()
	st, err := store.Open(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	q := st.CreateQuiz("Test quiz", "")
	_, err = st.UpdateQuiz(q.ID, func(q *model.Quiz) error {
		q.Questions = []model.Question{
			{
				ID: "q1", Text: "2+2?", TimeLimitSec: 20, Points: 1000, CorrectIndex: 1,
				Options: []model.Option{{Text: "3"}, {Text: "4"}, {Text: "5"}},
			},
			{
				ID: "q2", Text: "Capital of France?", TimeLimitSec: 10, Points: 1000, CorrectIndex: 0,
				Options: []model.Option{{Text: "Paris"}, {Text: "Lyon"}},
			},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed quiz: %v", err)
	}

	hb := hub.New()
	eng := New(st, hb)
	t.Cleanup(eng.Close)
	return eng, st, q
}

func TestJoinAssignsUniqueDisplayNames(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)

	a, err := eng.Join(sess.Code, "Rahul", "", "mobile", "10.0.0.1")
	if err != nil {
		t.Fatalf("first join: %v", err)
	}
	b, err := eng.Join(sess.Code, "rahul", "", "mobile", "10.0.0.2")
	if err != nil {
		t.Fatalf("second join: %v", err)
	}
	c, err := eng.Join(sess.Code, "Rahul", "", "mobile", "10.0.0.3")
	if err != nil {
		t.Fatalf("third join: %v", err)
	}
	if a.Name != "Rahul" {
		t.Errorf("first joiner name = %q, want %q", a.Name, "Rahul")
	}
	if b.Name != "rahul (2)" {
		t.Errorf("duplicate name = %q, want %q", b.Name, "rahul (2)")
	}
	if c.Name != "Rahul (3)" {
		t.Errorf("third duplicate = %q, want %q", c.Name, "Rahul (3)")
	}
	if a.Token == b.Token {
		t.Error("participants must get distinct rejoin tokens")
	}
}

func TestRejoinWithTokenKeepsScore(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	p, _ := eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")

	if err := eng.Control(sess.Code, "start"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := eng.Answer(sess.Code, p.ID, 0, 1); err != nil {
		t.Fatalf("answer: %v", err)
	}
	scoreBefore := p.Score
	if scoreBefore <= 0 {
		t.Fatalf("expected points for a correct answer, got %d", scoreBefore)
	}

	// The phone drops off Wi-Fi and comes back with its stored token.
	again, err := eng.Join(sess.Code, "Ada", p.Token, "mobile", "10.0.0.9")
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if again.ID != p.ID {
		t.Errorf("rejoin created a new participant (%s vs %s)", again.ID, p.ID)
	}
	if again.Score != scoreBefore {
		t.Errorf("rejoin score = %d, want %d", again.Score, scoreBefore)
	}
	if n := len(mustSession(t, st, sess.Code).Participants); n != 1 {
		t.Errorf("participant count after rejoin = %d, want 1", n)
	}
}

func TestLockedRoomBlocksNewJoinsButNotRejoins(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	p, _ := eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")

	if err := eng.Control(sess.Code, "lock"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := eng.Join(sess.Code, "Grace", "", "mobile", "10.0.0.2"); err != ErrLocked {
		t.Errorf("new join on locked room = %v, want ErrLocked", err)
	}
	if _, err := eng.Join(sess.Code, "Ada", p.Token, "mobile", "10.0.0.1"); err != nil {
		t.Errorf("rejoin on locked room should succeed, got %v", err)
	}
}

func TestNoParticipantCap(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	const n = 300
	for i := 0; i < n; i++ {
		// Distinct IPs: the join rate limiter is a per-address abuse guard, not
		// a participant cap, and must never turn a big room away.
		ip := "10.1." + itoa(i/250) + "." + itoa(i%250)
		if _, err := eng.Join(sess.Code, "Player"+itoa(i), "", "mobile", ip); err != nil {
			t.Fatalf("join %d failed: %v", i, err)
		}
	}
	if got := len(mustSession(t, st, sess.Code).Participants); got != n {
		t.Errorf("joined %d participants, want %d", got, n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestAnswerIsFinalAndValidated(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	p, _ := eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")

	if _, err := eng.Answer(sess.Code, p.ID, 0, 1); err == nil {
		t.Error("answering before the quiz starts should fail")
	}
	_ = eng.Control(sess.Code, "start")

	if _, err := eng.Answer(sess.Code, p.ID, 0, 99); err == nil {
		t.Error("out-of-range option should be rejected")
	}
	if _, err := eng.Answer(sess.Code, p.ID, 5, 0); err != ErrBadPhase {
		t.Error("answering a question that is not live should be rejected")
	}
	if _, err := eng.Answer(sess.Code, p.ID, 0, 0); err != nil {
		t.Fatalf("first answer: %v", err)
	}
	if _, err := eng.Answer(sess.Code, p.ID, 0, 1); err == nil {
		t.Error("changing a locked-in answer must be rejected")
	}
}

func TestEveryoneAnsweredEndsQuestionEarly(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	a, _ := eng.Join(sess.Code, "A", "", "mobile", "10.0.0.1")
	b, _ := eng.Join(sess.Code, "B", "", "mobile", "10.0.0.2")
	_ = eng.Control(sess.Code, "start")

	if _, err := eng.Answer(sess.Code, a.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
	if got := mustSession(t, st, sess.Code).Phase; got != model.PhaseQuestion {
		t.Fatalf("phase after one answer = %s, want question", got)
	}
	if _, err := eng.Answer(sess.Code, b.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := mustSession(t, st, sess.Code).Phase; got != model.PhaseReveal {
		t.Errorf("phase after everyone answered = %s, want reveal", got)
	}
}

func TestSpeedBonusScoring(t *testing.T) {
	q := model.Question{TimeLimitSec: 20, Points: 1000, CorrectIndex: 0}

	if got := scoreFor(q, true, false, 100); got != 0 {
		t.Errorf("wrong answer scored %d, want 0", got)
	}
	if got := scoreFor(q, false, true, 19000); got != 1000 {
		t.Errorf("flat scoring gave %d, want the full 1000 regardless of speed", got)
	}
	instant := scoreFor(q, true, true, 0)
	if instant != 1000 {
		t.Errorf("instant correct answer = %d, want 1000", instant)
	}
	half := scoreFor(q, true, true, 10000)
	if half != 750 {
		t.Errorf("half-time correct answer = %d, want 750", half)
	}
	late := scoreFor(q, true, true, 20000)
	if late != 500 {
		t.Errorf("last-moment correct answer = %d, want 500 (half guaranteed)", late)
	}
	if !(instant > half && half > late) {
		t.Error("speed bonus must decrease monotonically with elapsed time")
	}
}

func TestPauseAndResumePreservesRemainingTime(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	_, _ = eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")
	_ = eng.Control(sess.Code, "start")

	time.Sleep(120 * time.Millisecond)
	if err := eng.Control(sess.Code, "pause"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	s := mustSession(t, st, sess.Code)
	remain := s.PausedRemainMs
	if remain <= 0 || remain > 20000 {
		t.Fatalf("paused remaining = %d ms, want 0 < r <= 20000", remain)
	}

	time.Sleep(250 * time.Millisecond)
	if err := eng.Control(sess.Code, "resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	s = mustSession(t, st, sess.Code)
	after := s.DeadlineMs - nowMs()
	// The pause must not have eaten into the answering window.
	if diff := after - remain; diff > 60 || diff < -60 {
		t.Errorf("remaining time changed across pause by %d ms, want ~0", diff)
	}
	if s.Paused {
		t.Error("session still marked paused after resume")
	}
}

func TestRepeatQuestionClearsScores(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	p, _ := eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")
	_ = eng.Control(sess.Code, "start")
	if _, err := eng.Answer(sess.Code, p.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
	if p.Score == 0 {
		t.Fatal("expected a score before repeat")
	}
	if err := eng.Control(sess.Code, "repeat"); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	s := mustSession(t, st, sess.Code)
	if len(s.Answers) != 0 {
		t.Errorf("answers after repeat = %d, want 0", len(s.Answers))
	}
	if p.Score != 0 || p.Answered != 0 || p.Correct != 0 {
		t.Errorf("participant stats not rolled back: score=%d answered=%d correct=%d",
			p.Score, p.Answered, p.Correct)
	}
	// And the question must be answerable again.
	if _, err := eng.Answer(sess.Code, p.ID, 0, 1); err != nil {
		t.Errorf("answering a repeated question failed: %v", err)
	}
}

func TestFullSessionLifecycle(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	a, _ := eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")
	b, _ := eng.Join(sess.Code, "Bob", "", "desktop", "10.0.0.2")

	if mustSession(t, st, sess.Code).Phase != model.PhaseLobby {
		t.Fatal("new session should start in the lobby")
	}
	_ = eng.Control(sess.Code, "start")

	// Ada right, Bob wrong.
	_, _ = eng.Answer(sess.Code, a.ID, 0, 1)
	_, _ = eng.Answer(sess.Code, b.ID, 0, 0)
	if mustSession(t, st, sess.Code).Phase != model.PhaseReveal {
		t.Fatal("expected reveal once both answered")
	}

	_ = eng.Control(sess.Code, "next")
	s := mustSession(t, st, sess.Code)
	if s.Phase != model.PhaseQuestion || s.CurrentIndex != 1 {
		t.Fatalf("after next: phase=%s index=%d, want question/1", s.Phase, s.CurrentIndex)
	}

	// Only Ada answers question 2; Bob lets it time out.
	_, _ = eng.Answer(sess.Code, a.ID, 1, 0)
	_ = eng.Control(sess.Code, "next") // question -> reveal
	_ = eng.Control(sess.Code, "next") // reveal -> ended (last question)

	s = mustSession(t, st, sess.Code)
	if s.Phase != model.PhaseEnded {
		t.Fatalf("phase = %s, want ended", s.Phase)
	}
	if s.EndedAt.IsZero() {
		t.Error("EndedAt not recorded")
	}

	board := Standings(s)
	if len(board) != 2 {
		t.Fatalf("standings length = %d, want 2", len(board))
	}
	if board[0].Name != "Ada" || board[0].Rank != 1 {
		t.Errorf("winner = %s (rank %d), want Ada at rank 1", board[0].Name, board[0].Rank)
	}
	if board[1].Rank != 2 {
		t.Errorf("runner-up rank = %d, want 2", board[1].Rank)
	}
	if board[0].Score <= board[1].Score {
		t.Errorf("scores not ordered: %d vs %d", board[0].Score, board[1].Score)
	}
	if board[0].AccuracyPc != 100 {
		t.Errorf("Ada answered both correctly, accuracy = %d%%", board[0].AccuracyPc)
	}
	if board[1].AccuracyPc != 0 {
		t.Errorf("Bob answered one, wrongly, accuracy = %d%%", board[1].AccuracyPc)
	}
}

func TestVideoQuestionHoldsTheTimerUntilTheOrganiserIsReady(t *testing.T) {
	st, err := store.Open(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	q := st.CreateQuiz("Video quiz", "")
	_, _ = st.UpdateQuiz(q.ID, func(q *model.Quiz) error {
		q.Questions = []model.Question{{
			ID: "v1", Text: "What happened in the clip?", TimeLimitSec: 20, Points: 1000,
			CorrectIndex: 0, TimerAfterVid: true,
			Media:   &model.Media{ID: "clip.mp4", Kind: model.MediaVideo},
			Options: []model.Option{{Text: "A"}, {Text: "B"}},
		}}
		return nil
	})
	eng := New(st, hub.New())
	t.Cleanup(eng.Close)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	p, _ := eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")

	if err := eng.Control(sess.Code, "start"); err != nil {
		t.Fatalf("start: %v", err)
	}
	s := mustSession(t, st, sess.Code)
	if s.Phase != model.PhaseQuestion {
		t.Fatalf("phase = %s, want question (the video should be pushed immediately)", s.Phase)
	}
	if !s.Paused {
		t.Fatal("a 'start timer after video' question must open with the clock held")
	}
	if s.PausedRemainMs != 20000 {
		t.Errorf("held time = %d ms, want the full 20000", s.PausedRemainMs)
	}
	// Nobody can answer while the room is still watching.
	if _, err := eng.Answer(sess.Code, p.ID, 0, 0); err != ErrBadPhase {
		t.Errorf("answering during video playback = %v, want ErrBadPhase", err)
	}
	// The timer never runs down on its own while held.
	time.Sleep(400 * time.Millisecond)
	if s := mustSession(t, st, sess.Code); s.Phase != model.PhaseQuestion {
		t.Errorf("held question advanced by itself to %s", s.Phase)
	}

	if err := eng.Control(sess.Code, "resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	s = mustSession(t, st, sess.Code)
	if s.Paused {
		t.Error("still held after the organiser started the timer")
	}
	if remain := s.DeadlineMs - nowMs(); remain < 19000 {
		t.Errorf("remaining time after resume = %d ms, want the full window", remain)
	}
	if _, err := eng.Answer(sess.Code, p.ID, 0, 0); err != nil {
		t.Errorf("answering after the timer starts failed: %v", err)
	}
}

func TestAutoAdvanceMovesThroughTheQuizWithoutTheOrganiser(t *testing.T) {
	st, err := store.Open(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	q := st.CreateQuiz("Hands-off quiz", "")
	_, _ = st.UpdateQuiz(q.ID, func(q *model.Quiz) error {
		q.Settings.AutoAdvance = true
		q.Settings.RevealDurationMs = 300
		q.Questions = []model.Question{
			{ID: "a", Text: "One", TimeLimitSec: 5, Points: 1000, CorrectIndex: 0,
				Options: []model.Option{{Text: "x"}, {Text: "y"}}},
			{ID: "b", Text: "Two", TimeLimitSec: 5, Points: 1000, CorrectIndex: 0,
				Options: []model.Option{{Text: "x"}, {Text: "y"}}},
		}
		return nil
	})
	eng := New(st, hub.New())
	t.Cleanup(eng.Close)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	p, _ := eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")
	_ = eng.Control(sess.Code, "start")

	// Answering closes question 1; auto-advance should then carry the session
	// through the reveal into question 2 with no organiser input at all.
	if _, err := eng.Answer(sess.Code, p.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, st, sess.Code, func(s *model.Session) bool {
		return s.Phase == model.PhaseQuestion && s.CurrentIndex == 1
	}, "auto-advance to question 2")

	// Answering the last one should carry it all the way to the end.
	if _, err := eng.Answer(sess.Code, p.ID, 1, 0); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, st, sess.Code, func(s *model.Session) bool {
		return s.Phase == model.PhaseEnded
	}, "auto-advance to the end")
}

func waitPhase(t *testing.T, st *store.Store, code string, ok func(*model.Session) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := st.SessionSnapshot(code); err == nil && ok(s) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := st.SessionSnapshot(code)
	t.Fatalf("timed out waiting for %s (phase=%s index=%d)", what, s.Phase, s.CurrentIndex)
}

func TestStandingsTieBreakOnResponseTime(t *testing.T) {
	sess := &model.Session{
		Participants: []*model.Participant{
			{ID: "slow", Name: "Slow", Score: 1000, Answered: 1, Correct: 1, TotalMs: 9000},
			{ID: "fast", Name: "Fast", Score: 1000, Answered: 1, Correct: 1, TotalMs: 1200},
		},
	}
	board := standingsOf(sess)
	if board[0].ID != "fast" {
		t.Errorf("tie broken to %q, want the faster responder", board[0].ID)
	}
	if board[0].Rank != 1 || board[1].Rank != 2 {
		t.Errorf("ranks = %d,%d want 1,2", board[0].Rank, board[1].Rank)
	}
}

func TestCorrectAnswerHiddenFromParticipantsUntilReveal(t *testing.T) {
	eng, st, q := newTestEngine(t)
	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	_, _ = eng.Join(sess.Code, "Ada", "", "mobile", "10.0.0.1")
	_ = eng.Control(sess.Code, "start")

	st.RLock()
	s, _ := st.SessionLocked(sess.Code)
	quiz, _ := st.QuizLocked(sess.QuizID)
	partView := eng.snapshot(s, quiz, hub.RoleParticipant)
	hostView := eng.snapshot(s, quiz, hub.RoleHost)
	st.RUnlock()

	if partView.Question == nil {
		t.Fatal("participant should receive the live question")
	}
	if partView.Question.CorrectIndex != nil {
		t.Error("participants must not receive the correct index during a live question")
	}
	if partView.Reveal != nil {
		t.Error("participants must not receive answer counts during a live question")
	}
	if hostView.Question.CorrectIndex == nil {
		t.Error("organiser needs the correct index to run the question")
	}

	_ = eng.Control(sess.Code, "reveal")
	st.RLock()
	s, _ = st.SessionLocked(sess.Code)
	partView = eng.snapshot(s, quiz, hub.RoleParticipant)
	st.RUnlock()
	if partView.Question.CorrectIndex == nil {
		t.Error("participants should learn the answer at reveal")
	}
}

func TestRateLimiterCountsPerKey(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("attempt %d blocked too early", i+1)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Error("4th attempt from the same address should be blocked")
	}
	if !rl.allow("5.6.7.8") {
		t.Error("a different address must not be affected")
	}
}

// mustSession returns a detached snapshot. Reading the live object's fields
// would race with the engine's own timer goroutine.
func mustSession(t *testing.T, st *store.Store, code string) *model.Session {
	t.Helper()
	s, err := st.SessionSnapshot(code)
	if err != nil {
		t.Fatalf("session %s: %v", code, err)
	}
	return s
}
