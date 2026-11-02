package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwiqtest/internal/model"
)

func TestQuizAndSessionSurviveRestart(t *testing.T) {
	dir := t.TempDir()

	st, err := Open(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	q := st.CreateQuiz("Persisted quiz", "with a description")
	_, err = st.UpdateQuiz(q.ID, func(q *model.Quiz) error {
		q.HostToken = "host-token-1234567890abcdef"
		q.Questions = []model.Question{{
			ID: "q1", Text: "Is state durable?", TimeLimitSec: 30, Points: 1000, CorrectIndex: 0,
			Options: []model.Option{{Text: "Yes"}, {Text: "No"}},
		}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	sess := st.CreateSession(q.ID, q.HostToken, time.Hour)
	st.Lock()
	s, _ := st.SessionLocked(sess.Code)
	s.Participants = append(s.Participants, &model.Participant{
		ID: "p1", Token: "player-token-abc", Name: "Ada", Score: 750,
		Answered: 1, Correct: 1, TotalMs: 2500, JoinedAt: time.Now(), Connected: true,
	})
	s.Answers = append(s.Answers, model.Answer{
		ParticipantID: "p1", QuestionIndex: 0, OptionIndex: 0,
		Correct: true, Points: 750, ElapsedMs: 2500, AnsweredAt: time.Now(),
	})
	s.Phase = model.PhaseReveal
	s.CurrentIndex = 0
	st.MarkSessionLocked(sess.Code)
	st.Unlock()
	st.Close() // flushes

	// Reopen as a fresh process would.
	st2, err := Open(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	q2, err := st2.Quiz(q.ID)
	if err != nil {
		t.Fatalf("quiz lost across restart: %v", err)
	}
	if q2.Title != "Persisted quiz" || len(q2.Questions) != 1 {
		t.Errorf("quiz content changed: %+v", q2)
	}
	if q2.HostToken != "host-token-1234567890abcdef" {
		t.Error("host token not restored — the organiser would lose access to their own quiz")
	}

	s2, err := st2.Session(sess.Code)
	if err != nil {
		t.Fatalf("session lost across restart: %v", err)
	}
	if len(s2.Participants) != 1 || s2.Participants[0].Score != 750 {
		t.Errorf("participant scores not restored: %+v", s2.Participants)
	}
	if s2.Participants[0].Token != "player-token-abc" {
		t.Error("rejoin token not restored — a reconnecting phone would lose its score")
	}
	if s2.Participants[0].Connected {
		t.Error("nobody is connected right after a restart; the flag should be cleared")
	}
	if len(s2.Answers) != 1 || !s2.Answers[0].Correct || s2.Answers[0].ElapsedMs != 2500 {
		t.Errorf("answers not restored: %+v", s2.Answers)
	}
	if s2.Answers[0].AnsweredAt.IsZero() {
		t.Error("answer timestamp lost — the audit trail needs it")
	}
}

func TestMidQuestionRestartParksInReveal(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir, time.Hour)
	q := st.CreateQuiz("Interrupted", "")
	sess := st.CreateSession(q.ID, "tok", time.Hour)
	st.Lock()
	s, _ := st.SessionLocked(sess.Code)
	s.Phase = model.PhaseQuestion
	s.CurrentIndex = 0
	s.DeadlineMs = time.Now().Add(20 * time.Second).UnixMilli()
	st.MarkSessionLocked(sess.Code)
	st.Unlock()
	st.Close()

	st2, _ := Open(dir, time.Hour)
	defer st2.Close()
	s2, err := st2.Session(sess.Code)
	if err != nil {
		t.Fatal(err)
	}
	// A timer cannot be honestly resumed across a restart, so the session waits
	// for the organiser rather than silently scoring a question nobody saw.
	if s2.Phase != model.PhaseReveal || !s2.Paused {
		t.Errorf("after restart mid-question: phase=%s paused=%v, want reveal/paused", s2.Phase, s2.Paused)
	}
}

func TestExpiredSessionsArePurgedOnOpen(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir, time.Hour)
	q := st.CreateQuiz("Old", "")
	live := st.CreateSession(q.ID, "tok", time.Hour)
	stale := st.CreateSession(q.ID, "tok", time.Hour)
	st.Lock()
	s, _ := st.SessionLocked(stale.Code)
	s.ExpiresAt = time.Now().Add(-time.Minute)
	st.MarkSessionLocked(stale.Code)
	st.Unlock()
	st.Close()

	st2, _ := Open(dir, time.Hour)
	defer st2.Close()
	if _, err := st2.Session(live.Code); err != nil {
		t.Error("a session inside its retention window was purged")
	}
	if _, err := st2.Session(stale.Code); err != ErrNotFound {
		t.Error("an expired session should be deleted on load, for privacy")
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions", stale.Code+".json")); !os.IsNotExist(err) {
		t.Error("expired session file left on disk")
	}
}

func TestJoinCodesAreSixCharsAndUnique(t *testing.T) {
	st, _ := Open(t.TempDir(), time.Hour)
	defer st.Close()
	q := st.CreateQuiz("Codes", "")
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		s := st.CreateSession(q.ID, "tok", time.Hour)
		if len(s.Code) != 6 {
			t.Fatalf("code %q is not 6 characters", s.Code)
		}
		for _, r := range s.Code {
			if !strings.ContainsRune(codeAlphabet, r) {
				t.Fatalf("code %q contains a character outside the join-code alphabet", s.Code)
			}
		}
		if seen[s.Code] {
			t.Fatalf("duplicate join code issued: %s", s.Code)
		}
		seen[s.Code] = true
	}
}

func TestQuizzesAreScopedToTheirHost(t *testing.T) {
	st, _ := Open(t.TempDir(), time.Hour)
	defer st.Close()
	mine := st.CreateQuiz("Mine", "")
	_, _ = st.UpdateQuiz(mine.ID, func(q *model.Quiz) error { q.HostToken = "token-a"; return nil })
	theirs := st.CreateQuiz("Theirs", "")
	_, _ = st.UpdateQuiz(theirs.ID, func(q *model.Quiz) error { q.HostToken = "token-b"; return nil })

	list := st.QuizzesForHost("token-a")
	if len(list) != 1 || list[0].ID != mine.ID {
		t.Errorf("host A sees %d quizzes, want only their own", len(list))
	}
	if len(st.QuizzesForHost("")) != 0 {
		t.Error("an empty token must not match any quiz")
	}
}

func TestSaveMediaWritesUnderDataDir(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir, time.Hour)
	defer st.Close()

	id, err := st.SaveMedia(".png", []byte("not really a png"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(id) != ".png" {
		t.Errorf("media id %q lost its extension", id)
	}
	got, err := os.ReadFile(st.MediaPath(id))
	if err != nil {
		t.Fatalf("stored media unreadable: %v", err)
	}
	if string(got) != "not really a png" {
		t.Error("stored bytes do not match")
	}
	// Path traversal in an id must never escape the media directory.
	if p := st.MediaPath("../../etc/passwd"); filepath.Dir(p) != filepath.Join(dir, "media") {
		t.Errorf("MediaPath escaped the data directory: %s", p)
	}
}
