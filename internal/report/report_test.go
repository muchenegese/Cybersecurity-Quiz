package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
	"time"

	"kwiqtest/internal/model"
)

// fixture builds a small but complete session: 3 questions, 4 participants,
// including one who never answered and one who answered only part-way.
func fixture() (*model.Quiz, *model.Session) {
	start := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	quiz := &model.Quiz{
		ID: "quiz1", Title: "Geography Challenge",
		Description: "A quick tour of the world",
		Settings:    model.Settings{SpeedBonus: true},
		Questions: []model.Question{
			{ID: "q1", Text: "Capital of France?", TimeLimitSec: 20, Points: 1000, CorrectIndex: 0,
				Options: []model.Option{{Text: "Paris"}, {Text: "Lyon"}, {Text: "Nice"}}},
			{ID: "q2", Text: "Largest ocean?", TimeLimitSec: 15, Points: 1000, CorrectIndex: 1,
				Options: []model.Option{{Text: "Atlantic"}, {Text: "Pacific"}}},
			{ID: "q3", Text: "Which landmark is this?", TimeLimitSec: 30, Points: 2000, CorrectIndex: 2,
				Media:   &model.Media{ID: "abc.jpg", Kind: model.MediaImage, Name: "eiffel.jpg"},
				Options: []model.Option{{Text: "Big Ben"}, {Text: "Colosseum"}, {Text: "Eiffel Tower"}, {Text: "Sagrada Familia"}}},
		},
	}
	people := []struct {
		id, name, device string
	}{
		{"p1", "Ada Lovelace", "mobile"},
		{"p2", "Grace Hopper", "desktop"},
		{"p3", "Alan Turing", "tablet"},
		{"p4", "Silent Sam", "mobile"}, // joins, never answers
	}
	sess := &model.Session{
		Code: "482913", QuizID: quiz.ID,
		CreatedAt: start.Add(-5 * time.Minute), StartedAt: start,
		EndedAt: start.Add(4 * time.Minute), ExpiresAt: start.Add(72 * time.Hour),
		Phase: model.PhaseEnded, CurrentIndex: 2,
	}
	for i, p := range people {
		sess.Participants = append(sess.Participants, &model.Participant{
			ID: p.id, Name: p.name, Avatar: "🦊", Device: p.device,
			JoinedAt: start.Add(time.Duration(-i) * time.Minute),
		})
	}
	add := func(pid string, qi, oi int, elapsed int64) {
		q := quiz.Questions[qi]
		correct := oi == q.CorrectIndex
		pts := 0
		if correct {
			pts = q.Points
		}
		sess.Answers = append(sess.Answers, model.Answer{
			ParticipantID: pid, QuestionIndex: qi, OptionIndex: oi,
			Correct: correct, Points: pts, ElapsedMs: elapsed,
			AnsweredAt: start.Add(time.Duration(qi)*time.Minute + time.Duration(elapsed)*time.Millisecond),
		})
		for _, part := range sess.Participants {
			if part.ID == pid {
				part.Score += pts
				part.Answered++
				part.TotalMs += elapsed
				if correct {
					part.Correct++
				}
			}
		}
	}
	add("p1", 0, 0, 2400)  // Ada: right
	add("p2", 0, 1, 5100)  // Grace: wrong
	add("p3", 0, 0, 8200)  // Alan: right
	add("p1", 1, 1, 1800)  // Ada: right
	add("p2", 1, 1, 3300)  // Grace: right
	add("p1", 2, 2, 9000)  // Ada: right
	add("p3", 2, 0, 12000) // Alan: wrong
	return quiz, sess
}

func readCSV(t *testing.T, gen func(*bytes.Buffer) error) [][]string {
	t.Helper()
	var buf bytes.Buffer
	if err := gen(&buf); err != nil {
		t.Fatalf("generate: %v", err)
	}
	b := buf.Bytes()
	if !bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		t.Error("CSV is missing the UTF-8 BOM Excel needs to decode names correctly")
	}
	rows, err := csv.NewReader(bytes.NewReader(b[3:])).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	return rows
}

func TestLeaderboardCSVRanksEveryone(t *testing.T) {
	quiz, sess := fixture()
	rows := readCSV(t, func(b *bytes.Buffer) error { return LeaderboardCSV(b, quiz, sess) })

	if len(rows) != len(sess.Participants)+1 {
		t.Fatalf("got %d rows, want %d (header + one per participant)", len(rows), len(sess.Participants)+1)
	}
	if rows[1][1] != "Ada Lovelace" {
		t.Errorf("top of leaderboard = %q, want Ada Lovelace", rows[1][1])
	}
	if rows[1][0] != "1" {
		t.Errorf("top rank = %q, want 1", rows[1][0])
	}
	// Silent Sam answered nothing and must still appear, ranked last.
	last := rows[len(rows)-1]
	if last[1] != "Silent Sam" || last[2] != "0" {
		t.Errorf("non-answering participant missing or mis-scored: %v", last)
	}
}

func TestQuestionsCSVCoversEveryQuestionForEveryParticipant(t *testing.T) {
	quiz, sess := fixture()
	rows := readCSV(t, func(b *bytes.Buffer) error { return QuestionsCSV(b, quiz, sess) })

	want := len(quiz.Questions)*len(sess.Participants) + 1
	if len(rows) != want {
		t.Fatalf("got %d rows, want %d — the question-wise report must be exhaustive", len(rows), want)
	}
	// Every unanswered cell is explicit rather than blank.
	misses := 0
	for _, r := range rows[1:] {
		if strings.Contains(r[7], "no answer") {
			misses++
		}
	}
	// Sam missed 3, Grace missed q3, Alan missed q2 => 5.
	if misses != 5 {
		t.Errorf("explicit 'no answer' cells = %d, want 5", misses)
	}
	if !strings.Contains(rows[0][3], "Correct Answer") {
		t.Errorf("header missing the correct-answer column: %v", rows[0])
	}
}

func TestAttendeeCSVRecordsCompletionAndDevice(t *testing.T) {
	quiz, sess := fixture()
	rows := readCSV(t, func(b *bytes.Buffer) error { return AttendeesCSV(b, quiz, sess) })
	if len(rows) != len(sess.Participants)+1 {
		t.Fatalf("got %d rows, want %d", len(rows), len(sess.Participants)+1)
	}
	byName := map[string][]string{}
	for _, r := range rows[1:] {
		byName[r[0]] = r
	}
	if got := byName["Ada Lovelace"][3]; got != "completed" {
		t.Errorf("Ada answered all 3: status = %q, want completed", got)
	}
	if got := byName["Silent Sam"][3]; !strings.Contains(got, "no answers") {
		t.Errorf("Sam never answered: status = %q", got)
	}
	if got := byName["Grace Hopper"][3]; !strings.Contains(got, "partial") {
		t.Errorf("Grace answered 2 of 3: status = %q", got)
	}
	if got := byName["Alan Turing"][2]; got != "tablet" {
		t.Errorf("device not recorded: %q", got)
	}
}

func TestResponsesCSVIsChronological(t *testing.T) {
	quiz, sess := fixture()
	rows := readCSV(t, func(b *bytes.Buffer) error { return ResponsesCSV(b, quiz, sess) })
	if len(rows) != len(sess.Answers)+1 {
		t.Fatalf("got %d rows, want %d (one per recorded answer)", len(rows), len(sess.Answers)+1)
	}
	prev := ""
	for _, r := range rows[1:] {
		if prev != "" && r[0] < prev {
			t.Errorf("responses out of order: %q after %q", r[0], prev)
		}
		prev = r[0]
	}
}

// TestPDFContainsFullDetail is the report requirement the PRD is most emphatic
// about: the combined PDF must not omit any question, participant or response
// for the sake of brevity.
func TestPDFContainsFullDetail(t *testing.T) {
	quiz, sess := fixture()
	d := buildDoc(quiz, sess, "")
	d.pdf.SetCompression(false) // so the text is greppable in the output
	var buf bytes.Buffer
	if err := d.pdf.Output(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if !strings.HasPrefix(out, "%PDF-") {
		t.Fatal("output is not a PDF")
	}
	if buf.Len() < 3000 {
		t.Fatalf("PDF is only %d bytes — far too small to be a full report", buf.Len())
	}

	for _, p := range sess.Participants {
		if !strings.Contains(out, pdfEscape(p.Name)) {
			t.Errorf("participant %q missing from the PDF", p.Name)
		}
	}
	for _, q := range quiz.Questions {
		if !strings.Contains(out, pdfEscape(q.Text)) {
			t.Errorf("question %q missing from the PDF", q.Text)
		}
		for _, o := range q.Options {
			if !strings.Contains(out, pdfEscape(o.Text)) {
				t.Errorf("option %q of %q missing from the PDF", o.Text, q.Text)
			}
		}
	}
	for _, section := range []string{
		"Final leaderboard", "Question set", "Per-participant response breakdown", "Attendee list",
	} {
		if !strings.Contains(out, pdfEscape(section)) {
			t.Errorf("section %q missing from the PDF", section)
		}
	}
	for _, meta := range []string{sess.Code, "Duration", "Scoring mode", "Total points available"} {
		if !strings.Contains(out, pdfEscape(meta)) {
			t.Errorf("metadata %q missing from the PDF", meta)
		}
	}
	// Per-answer timings must be present, not just totals.
	if !strings.Contains(out, "2.40s") {
		t.Error("individual response times are missing from the PDF")
	}
	if strings.Count(out, pdfEscape("Capital of France?")) < 2 {
		t.Error("question text should appear in both the question set and each participant's breakdown")
	}
}

// pdfEscape mirrors the escaping fpdf applies inside a text string literal.
func pdfEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return r.Replace(s)
}

func TestPDFHandlesEmptySession(t *testing.T) {
	quiz, _ := fixture()
	empty := &model.Session{Code: "000111", QuizID: quiz.ID, Phase: model.PhaseEnded}
	var buf bytes.Buffer
	if err := PDF(&buf, quiz, empty, ""); err != nil {
		t.Fatalf("a session nobody joined should still produce a report: %v", err)
	}
	if buf.Len() < 1000 {
		t.Errorf("empty-session PDF is %d bytes", buf.Len())
	}
}

func TestPDFTextSanitisation(t *testing.T) {
	quiz, sess := fixture()
	d := newDoc(quiz, sess)
	// txt finishes with the cp1252 translation the core fonts require, so the
	// expectations below are written through that same translator.
	cases := map[string]string{
		"Plain ASCII":      "Plain ASCII",
		"café":             "café",       // accented Latin survives
		"“quoted”":         "\"quoted\"", // smart quotes normalised
		"dash — here":      "dash - here",
		"ellipsis…":        "ellipsis...",
		"emoji \U0001F98A": "emoji ", // dropped, not turned into noise
		"中文":               "??",     // unsupported script degrades visibly
	}
	for in, want := range cases {
		if got, exp := d.txt(in), d.tr(want); got != exp {
			t.Errorf("txt(%q) = %q, want %q", in, got, exp)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "—"},
		{45 * time.Second, "0m 45s"},
		{4*time.Minute + 5*time.Second, "4m 05s"},
		{time.Hour + 2*time.Minute, "1h 02m 00s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOptionLabelHandlesMissingAndMediaAnswers(t *testing.T) {
	q := model.Question{Options: []model.Option{
		{Text: "Paris"},
		{Media: &model.Media{Kind: model.MediaImage, Name: "tower.jpg"}},
	}}
	if got := optionLabel(q, 0); got != "A. Paris" {
		t.Errorf("text option = %q", got)
	}
	if got := optionLabel(q, 1); !strings.Contains(got, "tower.jpg") {
		t.Errorf("picture option should name its file, got %q", got)
	}
	if got := optionLabel(q, -1); !strings.Contains(got, "no answer") {
		t.Errorf("timed-out answer = %q", got)
	}
	if got := optionLabel(q, 99); !strings.Contains(got, "no answer") {
		t.Errorf("out-of-range option = %q", got)
	}
}

var _ = fmt.Sprintf
