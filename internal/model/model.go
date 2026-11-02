// Package model holds the core domain types for KwiqTest.
//
// Everything here is plain data that serialises cleanly to JSON: the whole
// persistence layer is "write these structs to a file", which is what keeps the
// app portable (no database engine, no CGO, no server to install).
package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------- Quiz ------

// MediaKind distinguishes the two upload types the PRD allows. Media is always
// uploaded from the local drive; there is deliberately no URL-ingestion field.
type MediaKind string

const (
	MediaNone  MediaKind = ""
	MediaImage MediaKind = "image"
	MediaVideo MediaKind = "video"
)

// Media references an uploaded asset stored under the data directory.
type Media struct {
	ID    string    `json:"id"`
	Kind  MediaKind `json:"kind"`
	Mime  string    `json:"mime"`
	Name  string    `json:"name"`
	Size  int64     `json:"size"`
	Width int       `json:"width,omitempty"`
	Heigh int       `json:"height,omitempty"`
}

// URL is the public path the browser fetches this asset from.
func (m *Media) URL() string {
	if m == nil || m.ID == "" {
		return ""
	}
	return "/media/" + m.ID
}

// Option is a single answer choice. It may carry its own media so organisers
// can build "pick the right picture" questions.
type Option struct {
	Text  string `json:"text"`
	Media *Media `json:"media,omitempty"`
}

// Question is a single multiple-choice question.
type Question struct {
	ID            string   `json:"id"`
	Text          string   `json:"text"`
	Options       []Option `json:"options"`
	CorrectIndex  int      `json:"correctIndex"`
	TimeLimitSec  int      `json:"timeLimitSec"`
	Points        int      `json:"points"`
	Media         *Media   `json:"media,omitempty"`
	TimerAfterVid bool     `json:"timerAfterVideo"`
}

// Settings are the organiser-tunable knobs for how a session plays out.
type Settings struct {
	SpeedBonus       bool `json:"speedBonus"`       // faster correct answers score higher
	ShowLeaderboard  bool `json:"showLeaderboard"`  // show standings to participants between questions
	ShowLiveRank     bool `json:"showLiveRank"`     // show own rank on the post-answer screen
	AutoAdvance      bool `json:"autoAdvance"`      // move on automatically when the timer expires
	RevealDurationMs int  `json:"revealDurationMs"` // how long the reveal screen holds on auto-advance
}

// DefaultSettings returns the settings a freshly created quiz starts with.
func DefaultSettings() Settings {
	return Settings{
		SpeedBonus:       true,
		ShowLeaderboard:  true,
		ShowLiveRank:     true,
		AutoAdvance:      false,
		RevealDurationMs: 6000,
	}
}

// Quiz is the authored content: title, questions, and playback settings.
type Quiz struct {
	ID          string     `json:"id"`
	HostToken   string     `json:"-"` // secret; never leaves the server in quiz payloads
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Cover       *Media     `json:"cover,omitempty"`
	Questions   []Question `json:"questions"`
	Settings    Settings   `json:"settings"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	IsTemplate  bool       `json:"isTemplate"`
}

// Validate enforces the PRD's publish-time acceptance criteria.
func (q *Quiz) Validate() error {
	if strings.TrimSpace(q.Title) == "" {
		return errors.New("quiz needs a title")
	}
	if len(q.Questions) == 0 {
		return errors.New("add at least one question before launching a session")
	}
	for i, qs := range q.Questions {
		n := i + 1
		if strings.TrimSpace(qs.Text) == "" && qs.Media == nil {
			return fmt.Errorf("question %d needs text or a media prompt", n)
		}
		if len(qs.Options) < 2 || len(qs.Options) > 6 {
			return fmt.Errorf("question %d must have between 2 and 6 options", n)
		}
		for j, op := range qs.Options {
			if strings.TrimSpace(op.Text) == "" && op.Media == nil {
				return fmt.Errorf("question %d, option %s is empty", n, string(rune('A'+j)))
			}
		}
		if qs.CorrectIndex < 0 || qs.CorrectIndex >= len(qs.Options) {
			return fmt.Errorf("question %d has no correct option selected", n)
		}
		if qs.TimeLimitSec < 5 || qs.TimeLimitSec > 120 {
			return fmt.Errorf("question %d time limit must be 5-120 seconds", n)
		}
		if qs.Points < 0 || qs.Points > 10000 {
			return fmt.Errorf("question %d has an invalid points value", n)
		}
	}
	return nil
}

// ------------------------------------------------------------- Session ------

// Phase is the session state machine's current state.
type Phase string

const (
	PhaseLobby    Phase = "lobby"    // waiting for the organiser to start
	PhaseQuestion Phase = "question" // a question is live, timer running
	PhaseReveal   Phase = "reveal"   // answers locked, showing the correct option
	PhaseEnded    Phase = "ended"    // finished; reports available
)

// Participant is one joined attendee. No account, no PII beyond a display name.
type Participant struct {
	ID         string    `json:"id"`
	Token      string    `json:"-"` // secret rejoin credential
	Name       string    `json:"name"`
	Avatar     string    `json:"avatar"` // emoji, assigned on join
	JoinedAt   time.Time `json:"joinedAt"`
	Device     string    `json:"device"` // "mobile" | "tablet" | "desktop"
	Score      int       `json:"score"`
	Correct    int       `json:"correct"`
	TotalMs    int64     `json:"totalMs"` // cumulative response time, the tie-breaker
	Answered   int       `json:"answered"`
	Connected  bool      `json:"connected"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

// Answer records one participant's response to one question.
type Answer struct {
	ParticipantID string `json:"participantId"`
	QuestionIndex int    `json:"questionIndex"`
	OptionIndex   int    `json:"optionIndex"` // -1 means "ran out of time"
	Correct       bool   `json:"correct"`
	Points        int    `json:"points"`
	ElapsedMs     int64  `json:"elapsedMs"`
	AnsweredAt    time.Time
}

// Session is one live run of a quiz.
type Session struct {
	Code      string    `json:"code"`      // 6-char alphanumeric join code (A-Z, 2-9), also the URL slug
	QuizID    string    `json:"quizId"`    //
	HostToken string    `json:"-"`         // organiser's secret for this session
	CreatedAt time.Time `json:"createdAt"` //
	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`

	Phase        Phase `json:"phase"`
	CurrentIndex int   `json:"currentIndex"` // -1 while in the lobby
	Locked       bool  `json:"locked"`       // organiser stopped further joins

	// Server-authoritative timing. Clients never trust their own clock; they
	// render a countdown against DeadlineMs corrected by a measured offset.
	QuestionStartMs int64 `json:"questionStartMs"`
	DeadlineMs      int64 `json:"deadlineMs"`
	PausedRemainMs  int64 `json:"pausedRemainMs"` // >0 while paused
	Paused          bool  `json:"paused"`

	Participants []*Participant `json:"participants"`
	Answers      []Answer       `json:"answers"`
}

// Standing is one row of the leaderboard.
type Standing struct {
	Rank       int    `json:"rank"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Avatar     string `json:"avatar"`
	Score      int    `json:"score"`
	Correct    int    `json:"correct"`
	Answered   int    `json:"answered"`
	AvgMs      int64  `json:"avgMs"`
	AccuracyPc int    `json:"accuracyPc"`
}
