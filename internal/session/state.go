package session

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"

	"kwiqtest/internal/hub"
	"kwiqtest/internal/model"
)

// ---------------------------------------------------------- wire payloads ---

type optionPayload struct {
	Text     string `json:"text"`
	Media    string `json:"media,omitempty"`
	MediaKnd string `json:"mediaKind,omitempty"`
}

type questionPayload struct {
	Text            string          `json:"text"`
	Media           string          `json:"media,omitempty"`
	MediaKind       string          `json:"mediaKind,omitempty"`
	MediaMime       string          `json:"mediaMime,omitempty"`
	Options         []optionPayload `json:"options"`
	TimeLimitSec    int             `json:"timeLimitSec"`
	Points          int             `json:"points"`
	StartMs         int64           `json:"startMs"`
	DeadlineMs      int64           `json:"deadlineMs"`
	RemainMs        int64           `json:"remainMs"`
	Paused          bool            `json:"paused"`
	TimerAfterVideo bool            `json:"timerAfterVideo"`
	// CorrectIndex is withheld from participants until the reveal phase.
	CorrectIndex *int `json:"correctIndex,omitempty"`
}

type revealPayload struct {
	CorrectIndex int   `json:"correctIndex"`
	Counts       []int `json:"counts"`
	Answered     int   `json:"answered"`
	NoAnswer     int   `json:"noAnswer"`
}

type participantPayload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Avatar    string `json:"avatar"`
	Score     int    `json:"score"`
	Connected bool   `json:"connected"`
	Answered  bool   `json:"answeredThis"`
	Device    string `json:"device"`
}

type settingsPayload struct {
	SpeedBonus      bool `json:"speedBonus"`
	ShowLeaderboard bool `json:"showLeaderboard"`
	ShowLiveRank    bool `json:"showLiveRank"`
	AutoAdvance     bool `json:"autoAdvance"`
}

type youPayload struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar"`
	Score    int    `json:"score"`
	Rank     int    `json:"rank"`
	Of       int    `json:"of"`
	Answered bool   `json:"answered"`
	Selected int    `json:"selected"`
	Correct  bool   `json:"correct"`
	Gained   int    `json:"gained"`
}

type statePayload struct {
	Type             string               `json:"type"`
	ServerNow        int64                `json:"serverNow"`
	Code             string               `json:"code"`
	QuizID           string               `json:"quizId,omitempty"`
	Phase            string               `json:"phase"`
	Title            string               `json:"title"`
	Description      string               `json:"description,omitempty"`
	Index            int                  `json:"index"`
	Total            int                  `json:"total"`
	Locked           bool                 `json:"locked"`
	ParticipantCount int                  `json:"participantCount"`
	AnsweredCount    int                  `json:"answeredCount"`
	Question         *questionPayload     `json:"question,omitempty"`
	Reveal           *revealPayload       `json:"reveal,omitempty"`
	Leaderboard      []model.Standing     `json:"leaderboard,omitempty"`
	Participants     []participantPayload `json:"participants,omitempty"`
	Settings         settingsPayload      `json:"settings"`
	Duration         int64                `json:"durationMs,omitempty"`
}

// ------------------------------------------------------------- publishing ---

// Publish broadcasts current state to a room. When hostOnly is set, only the
// organiser and big-screen views are refreshed — used for high-frequency
// answer-count updates that participants must not see.
func (e *Engine) Publish(code string, hostOnly bool) {
	e.st.RLock()
	sess, ok := e.st.SessionLocked(code)
	if !ok {
		e.st.RUnlock()
		return
	}
	quiz, ok := e.st.QuizLocked(sess.QuizID)
	if !ok {
		e.st.RUnlock()
		return
	}
	hostState := e.snapshot(sess, quiz, hub.RoleHost)
	screenState := e.snapshot(sess, quiz, hub.RoleScreen)
	partState := e.snapshot(sess, quiz, hub.RoleParticipant)

	// Personal data for each participant is computed once here, under the read
	// lock, and stitched onto the shared payload during the broadcast.
	you := map[string]youPayload{}
	if !hostOnly {
		standings := standingsOf(sess)
		rankByID := map[string]int{}
		for _, s := range standings {
			rankByID[s.ID] = s.Rank
		}
		ansByID := map[string]model.Answer{}
		for _, a := range sess.Answers {
			if a.QuestionIndex == sess.CurrentIndex {
				ansByID[a.ParticipantID] = a
			}
		}
		for _, p := range sess.Participants {
			y := youPayload{
				ID: p.ID, Name: p.Name, Avatar: p.Avatar, Score: p.Score,
				Rank: rankByID[p.ID], Of: len(sess.Participants), Selected: -1,
			}
			if a, ok := ansByID[p.ID]; ok {
				y.Answered, y.Selected, y.Correct, y.Gained = true, a.OptionIndex, a.Correct, a.Points
			}
			you[p.ID] = y
		}
	}
	e.st.RUnlock()

	hostFrame := sseFrame("state", hostState)
	screenFrame := sseFrame("state", screenState)
	partPrefix := jsonPrefix(partState)

	e.hb.Broadcast(code, func(s *hub.Sub) hub.Frame {
		switch s.Role {
		case hub.RoleHost:
			return hostFrame
		case hub.RoleScreen:
			return screenFrame
		default:
			if hostOnly {
				return nil
			}
			y, ok := you[s.ParticipantID]
			if !ok {
				return sseFrame("state", partState)
			}
			yb, _ := json.Marshal(y)
			var b bytes.Buffer
			b.WriteString("event: state\ndata: ")
			b.Write(partPrefix)
			b.WriteString(`,"you":`)
			b.Write(yb)
			b.WriteString("}\n\n")
			return b.Bytes()
		}
	})
}

// PublishParticipant refreshes one participant without touching anyone else.
//
// This is what makes an answer feel instant in a big room: the person who just
// tapped gets their own updated state immediately, while the other 499 devices
// are left alone (they must not learn anything about this answer yet anyway).
func (e *Engine) PublishParticipant(code, participantID string) {
	frame := e.FrameFor(code, hub.RoleParticipant, participantID)
	if frame == nil {
		return
	}
	e.hb.Broadcast(code, func(s *hub.Sub) hub.Frame {
		if s.Role == hub.RoleParticipant && s.ParticipantID == participantID {
			return frame
		}
		return nil
	})
}

// FrameFor builds a complete state frame for a single client. It is used to
// prime a connection the moment it opens, so a participant who joins late — or
// whose phone dropped and reconnected — is immediately in sync without waiting
// for the next broadcast.
func (e *Engine) FrameFor(code string, role hub.Role, participantID string) hub.Frame {
	e.st.RLock()
	defer e.st.RUnlock()
	sess, ok := e.st.SessionLocked(code)
	if !ok {
		return nil
	}
	quiz, ok := e.st.QuizLocked(sess.QuizID)
	if !ok {
		return nil
	}
	st := e.snapshot(sess, quiz, role)
	if role != hub.RoleParticipant {
		return sseFrame("state", st)
	}
	y := buildYou(sess, participantID)
	yb, _ := json.Marshal(y)
	var b bytes.Buffer
	b.WriteString("event: state\ndata: ")
	b.Write(jsonPrefix(st))
	b.WriteString(`,"you":`)
	b.Write(yb)
	b.WriteString("}\n\n")
	return b.Bytes()
}

// buildYou assembles one participant's personal slice of state.
func buildYou(sess *model.Session, participantID string) youPayload {
	y := youPayload{Selected: -1, Of: len(sess.Participants)}
	for _, p := range sess.Participants {
		if p.ID != participantID {
			continue
		}
		y.ID, y.Name, y.Avatar, y.Score = p.ID, p.Name, p.Avatar, p.Score
		y.Rank = rankOf(sess, p.ID)
	}
	for _, a := range sess.Answers {
		if a.ParticipantID == participantID && a.QuestionIndex == sess.CurrentIndex {
			y.Answered, y.Selected, y.Correct, y.Gained = true, a.OptionIndex, a.Correct, a.Points
		}
	}
	return y
}

// snapshot renders session state from the perspective of one role.
func (e *Engine) snapshot(sess *model.Session, quiz *model.Quiz, role hub.Role) statePayload {
	privileged := role == hub.RoleHost || role == hub.RoleScreen
	st := statePayload{
		Type:             "state",
		ServerNow:        nowMs(),
		Code:             sess.Code,
		Phase:            string(sess.Phase),
		Title:            quiz.Title,
		Description:      quiz.Description,
		Index:            sess.CurrentIndex,
		Total:            len(quiz.Questions),
		Locked:           sess.Locked,
		ParticipantCount: len(sess.Participants),
		Settings: settingsPayload{
			SpeedBonus:      quiz.Settings.SpeedBonus,
			ShowLeaderboard: quiz.Settings.ShowLeaderboard,
			ShowLiveRank:    quiz.Settings.ShowLiveRank,
			AutoAdvance:     quiz.Settings.AutoAdvance,
		},
	}
	if !sess.StartedAt.IsZero() {
		end := sess.EndedAt
		if end.IsZero() {
			end = time.Now()
		}
		st.Duration = end.Sub(sess.StartedAt).Milliseconds()
	}

	idx := sess.CurrentIndex
	inQuestion := idx >= 0 && idx < len(quiz.Questions)
	if inQuestion {
		st.AnsweredCount = countAnswers(sess, idx)
	}

	if inQuestion && (sess.Phase == model.PhaseQuestion || sess.Phase == model.PhaseReveal) {
		q := quiz.Questions[idx]
		qp := &questionPayload{
			Text:            q.Text,
			Options:         make([]optionPayload, 0, len(q.Options)),
			TimeLimitSec:    q.TimeLimitSec,
			Points:          q.Points,
			StartMs:         sess.QuestionStartMs,
			DeadlineMs:      sess.DeadlineMs,
			Paused:          sess.Paused,
			TimerAfterVideo: q.TimerAfterVid,
		}
		if q.Media != nil {
			qp.Media, qp.MediaKind, qp.MediaMime = q.Media.URL(), string(q.Media.Kind), q.Media.Mime
		}
		for _, o := range q.Options {
			op := optionPayload{Text: o.Text}
			if o.Media != nil {
				op.Media, op.MediaKnd = o.Media.URL(), string(o.Media.Kind)
			}
			qp.Options = append(qp.Options, op)
		}
		if sess.Paused {
			qp.RemainMs = sess.PausedRemainMs
		} else {
			qp.RemainMs = sess.DeadlineMs - nowMs()
			if qp.RemainMs < 0 {
				qp.RemainMs = 0
			}
		}
		// The correct answer only ever crosses the wire once it can no longer
		// be used to cheat: to the organiser/projector, or at reveal.
		if privileged || sess.Phase == model.PhaseReveal {
			ci := q.CorrectIndex
			qp.CorrectIndex = &ci
		}
		st.Question = qp

		if sess.Phase == model.PhaseReveal || privileged {
			counts := make([]int, len(q.Options))
			answered := 0
			for _, a := range sess.Answers {
				if a.QuestionIndex == idx && a.OptionIndex >= 0 && a.OptionIndex < len(counts) {
					counts[a.OptionIndex]++
					answered++
				}
			}
			st.Reveal = &revealPayload{
				CorrectIndex: q.CorrectIndex,
				Counts:       counts,
				Answered:     answered,
				NoAnswer:     max0(len(sess.Participants) - answered),
			}
		}
	}

	showBoard := privileged ||
		sess.Phase == model.PhaseEnded ||
		(sess.Phase == model.PhaseReveal && quiz.Settings.ShowLeaderboard)
	if showBoard {
		board := standingsOf(sess)
		if !privileged && len(board) > 10 {
			board = board[:10] // participants see the top 10; hosts see everyone
		}
		st.Leaderboard = board
	}

	if privileged {
		st.QuizID = sess.QuizID
		answeredNow := map[string]bool{}
		for _, a := range sess.Answers {
			if a.QuestionIndex == idx {
				answeredNow[a.ParticipantID] = true
			}
		}
		st.Participants = make([]participantPayload, 0, len(sess.Participants))
		for _, p := range sess.Participants {
			st.Participants = append(st.Participants, participantPayload{
				ID: p.ID, Name: p.Name, Avatar: p.Avatar, Score: p.Score,
				Connected: p.Connected, Answered: answeredNow[p.ID], Device: p.Device,
			})
		}
	}
	return st
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// --------------------------------------------------------------- ranking ----

// standingsOf ranks participants by score, breaking ties on cumulative
// response time (faster wins) and then name, so ordering is deterministic.
func standingsOf(sess *model.Session) []model.Standing {
	out := make([]model.Standing, 0, len(sess.Participants))
	for _, p := range sess.Participants {
		s := model.Standing{
			ID: p.ID, Name: p.Name, Avatar: p.Avatar,
			Score: p.Score, Correct: p.Correct, Answered: p.Answered,
		}
		if p.Answered > 0 {
			s.AvgMs = p.TotalMs / int64(p.Answered)
			s.AccuracyPc = int(float64(p.Correct)/float64(p.Answered)*100 + 0.5)
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		ti, tj := totalMsOf(sess, out[i].ID), totalMsOf(sess, out[j].ID)
		if ti != tj {
			return ti < tj
		}
		return out[i].Name < out[j].Name
	})
	for i := range out {
		out[i].Rank = i + 1
		// Equal score and equal cumulative time share a rank.
		if i > 0 && out[i].Score == out[i-1].Score &&
			totalMsOf(sess, out[i].ID) == totalMsOf(sess, out[i-1].ID) {
			out[i].Rank = out[i-1].Rank
		}
	}
	return out
}

func totalMsOf(sess *model.Session, id string) int64 {
	for _, p := range sess.Participants {
		if p.ID == id {
			return p.TotalMs
		}
	}
	return 0
}

func rankOf(sess *model.Session, id string) int {
	for _, s := range standingsOf(sess) {
		if s.ID == id {
			return s.Rank
		}
	}
	return 0
}

// Standings is the exported ranking used by the report generators.
func Standings(sess *model.Session) []model.Standing { return standingsOf(sess) }

// ------------------------------------------------------------- encoding -----

func sseFrame(event string, v any) hub.Frame {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out bytes.Buffer
	out.Grow(len(b) + len(event) + 20)
	out.WriteString("event: ")
	out.WriteString(event)
	out.WriteString("\ndata: ")
	out.Write(b)
	out.WriteString("\n\n")
	return out.Bytes()
}

// jsonPrefix encodes v and strips the closing brace so a per-client field can
// be appended without re-encoding the shared payload for every participant.
func jsonPrefix(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil || len(b) < 2 || b[len(b)-1] != '}' {
		return []byte("{")
	}
	return b[:len(b)-1]
}
