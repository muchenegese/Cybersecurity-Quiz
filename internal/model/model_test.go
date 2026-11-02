package model

import (
	"strings"
	"testing"
)

func validQuiz() *Quiz {
	return &Quiz{
		Title: "Valid quiz",
		Questions: []Question{{
			Text: "Pick one", TimeLimitSec: 20, Points: 1000, CorrectIndex: 1,
			Options: []Option{{Text: "A"}, {Text: "B"}},
		}},
		Settings: DefaultSettings(),
	}
}

func TestValidateAcceptsAGoodQuiz(t *testing.T) {
	if err := validQuiz().Validate(); err != nil {
		t.Fatalf("valid quiz rejected: %v", err)
	}
}

func TestValidateRejectsPublishBlockers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Quiz)
		want   string
	}{
		{"no title", func(q *Quiz) { q.Title = "   " }, "title"},
		{"no questions", func(q *Quiz) { q.Questions = nil }, "at least one question"},
		{"empty question", func(q *Quiz) { q.Questions[0].Text = "" }, "text or a media prompt"},
		{"one option", func(q *Quiz) { q.Questions[0].Options = []Option{{Text: "A"}}; q.Questions[0].CorrectIndex = 0 }, "between 2 and 6"},
		{"seven options", func(q *Quiz) {
			q.Questions[0].Options = make([]Option, 7)
			for i := range q.Questions[0].Options {
				q.Questions[0].Options[i].Text = "x"
			}
		}, "between 2 and 6"},
		{"blank option", func(q *Quiz) { q.Questions[0].Options[1].Text = "" }, "is empty"},
		{"correct index out of range", func(q *Quiz) { q.Questions[0].CorrectIndex = 5 }, "no correct option"},
		{"negative correct index", func(q *Quiz) { q.Questions[0].CorrectIndex = -1 }, "no correct option"},
		{"timer too short", func(q *Quiz) { q.Questions[0].TimeLimitSec = 3 }, "5-120 seconds"},
		{"timer too long", func(q *Quiz) { q.Questions[0].TimeLimitSec = 300 }, "5-120 seconds"},
		{"absurd points", func(q *Quiz) { q.Questions[0].Points = 999999 }, "points"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := validQuiz()
			c.mutate(q)
			err := q.Validate()
			if err == nil {
				t.Fatalf("expected a validation error mentioning %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err.Error(), c.want)
			}
		})
	}
}

func TestMediaOnlyQuestionsAndOptionsAreValid(t *testing.T) {
	q := validQuiz()
	// "Which landmark is this?" with picture answers and no text anywhere.
	q.Questions[0].Text = ""
	q.Questions[0].Media = &Media{ID: "a.jpg", Kind: MediaImage}
	q.Questions[0].Options[0] = Option{Media: &Media{ID: "b.jpg", Kind: MediaImage}}
	q.Questions[0].Options[1] = Option{Media: &Media{ID: "c.jpg", Kind: MediaImage}}
	if err := q.Validate(); err != nil {
		t.Errorf("a picture-only question should be publishable: %v", err)
	}
}

func TestValidateReportsTheQuestionNumber(t *testing.T) {
	q := validQuiz()
	q.Questions = append(q.Questions, Question{
		Text: "Second", TimeLimitSec: 20, Points: 1000, CorrectIndex: 0,
		Options: []Option{{Text: "A"}},
	})
	err := q.Validate()
	if err == nil || !strings.Contains(err.Error(), "question 2") {
		t.Errorf("error should point at question 2, got %v", err)
	}
}

func TestMediaURL(t *testing.T) {
	var nilMedia *Media
	if nilMedia.URL() != "" {
		t.Error("nil media must yield an empty URL, not a broken link")
	}
	if (&Media{}).URL() != "" {
		t.Error("media with no id must yield an empty URL")
	}
	if got := (&Media{ID: "abc.jpg"}).URL(); got != "/media/abc.jpg" {
		t.Errorf("URL = %q", got)
	}
}

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if !s.SpeedBonus || !s.ShowLeaderboard {
		t.Error("a new quiz should default to the livelier Kahoot-style experience")
	}
	if s.AutoAdvance {
		t.Error("auto-advance should be off by default: the organiser paces the room")
	}
	if s.RevealDurationMs <= 0 {
		t.Error("reveal dwell time must be positive")
	}
}
