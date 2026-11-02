package report

import (
	"testing"
)

func TestBuildAnalyticsCountsAndStandings(t *testing.T) {
	quiz, sess := fixture()
	a := BuildAnalytics(quiz, sess)

	if a.Session.Code != sess.Code || a.Session.Title != quiz.Title {
		t.Fatalf("session meta = %+v, want code %q title %q", a.Session, sess.Code, quiz.Title)
	}
	if a.Stats.Participants != len(sess.Participants) {
		t.Errorf("Stats.Participants = %d, want %d", a.Stats.Participants, len(sess.Participants))
	}
	if a.Stats.Questions != len(quiz.Questions) {
		t.Errorf("Stats.Questions = %d, want %d", a.Stats.Questions, len(quiz.Questions))
	}
	if a.Stats.Responses != len(sess.Answers) {
		t.Errorf("Stats.Responses = %d, want %d", a.Stats.Responses, len(sess.Answers))
	}
	// 5 correct out of 7 total answers in the fixture (see report_test.go).
	if a.Stats.AccuracyPc != 71 {
		t.Errorf("Stats.AccuracyPc = %d, want 71", a.Stats.AccuracyPc)
	}

	if len(a.Standings) != len(sess.Participants) {
		t.Fatalf("Standings has %d rows, want %d", len(a.Standings), len(sess.Participants))
	}
	if a.Standings[0].Name != "Ada Lovelace" {
		t.Errorf("top standing = %q, want Ada Lovelace", a.Standings[0].Name)
	}
}

func TestBuildAnalyticsQuestionBreakdown(t *testing.T) {
	quiz, sess := fixture()
	a := BuildAnalytics(quiz, sess)

	if len(a.Questions) != len(quiz.Questions) {
		t.Fatalf("got %d questions, want %d", len(a.Questions), len(quiz.Questions))
	}
	q0 := a.Questions[0]
	if q0.Answered != 3 || q0.Correct != 2 {
		t.Errorf("question 0: answered=%d correct=%d, want 3/2", q0.Answered, q0.Correct)
	}
	if q0.NoAnswer != 1 {
		t.Errorf("question 0: noAnswer=%d, want 1 (Silent Sam)", q0.NoAnswer)
	}
	if len(q0.Options) != len(quiz.Questions[0].Options) {
		t.Fatalf("question 0 has %d options, want %d", len(q0.Options), len(quiz.Questions[0].Options))
	}
	// Two of three respondents to Q1 picked option A (Paris, the correct one).
	if !q0.Options[0].IsCorrect || q0.Options[0].Count != 2 {
		t.Errorf("question 0 option A = %+v, want correct with count 2", q0.Options[0])
	}
	if q0.Options[0].Pct != 67 {
		t.Errorf("question 0 option A pct = %d, want 67", q0.Options[0].Pct)
	}
}

func TestBuildAnalyticsParticipantAnswers(t *testing.T) {
	quiz, sess := fixture()
	a := BuildAnalytics(quiz, sess)

	var ada *ParticipantAnalysis
	for i := range a.Participants {
		if a.Participants[i].Name == "Ada Lovelace" {
			ada = &a.Participants[i]
		}
	}
	if ada == nil {
		t.Fatal("Ada Lovelace missing from Participants")
	}
	if ada.Rank != 1 {
		t.Errorf("Ada rank = %d, want 1", ada.Rank)
	}
	if len(ada.Answers) != len(quiz.Questions) {
		t.Fatalf("Ada has %d answer entries, want %d (one per question)", len(ada.Answers), len(quiz.Questions))
	}
	for _, ans := range ada.Answers {
		if !ans.Correct {
			t.Errorf("Ada answered every question correctly per the fixture, but Q%d shows wrong", ans.QuestionIndex+1)
		}
	}

	var sam *ParticipantAnalysis
	for i := range a.Participants {
		if a.Participants[i].Name == "Silent Sam" {
			sam = &a.Participants[i]
		}
	}
	if sam == nil {
		t.Fatal("Silent Sam missing from Participants")
	}
	if sam.Status == "" || sam.Answered != 0 {
		t.Errorf("Silent Sam should show zero answers, got status=%q answered=%d", sam.Status, sam.Answered)
	}
	for _, ans := range sam.Answers {
		if ans.Selected != -1 {
			t.Errorf("Silent Sam never answered; question %d should show Selected=-1, got %d", ans.QuestionIndex, ans.Selected)
		}
	}
}
