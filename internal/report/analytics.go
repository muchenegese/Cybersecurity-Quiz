package report

import (
	"kwiqtest/internal/model"
	"kwiqtest/internal/session"
)

// Analytics is the JSON-friendly counterpart to the PDF report: the same
// computed numbers (standings, per-question distributions, per-participant
// breakdowns), shaped for an interactive dashboard instead of a printed page.

// SessionMeta identifies the session and quiz an Analytics report describes.
type SessionMeta struct {
	Code        string `json:"code"`
	QuizID      string `json:"quizId"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Phase       string `json:"phase"`
	CreatedAt   string `json:"createdAt"`
	StartedAt   string `json:"startedAt,omitempty"`
	EndedAt     string `json:"endedAt,omitempty"`
	ExpiresAt   string `json:"expiresAt"`
}

// Stats are the headline numbers shown at the top of the dashboard.
type Stats struct {
	Participants int    `json:"participants"`
	Questions    int    `json:"questions"`
	Responses    int    `json:"responses"`
	AccuracyPc   int    `json:"accuracyPc"`
	DurationSec  int64  `json:"durationSec"`
	Duration     string `json:"duration"`
}

// OptionStat is one answer option's share of the responses to its question.
type OptionStat struct {
	Label     string `json:"label"` // "A", "B", ...
	Text      string `json:"text"`
	MediaURL  string `json:"mediaUrl,omitempty"`
	Count     int    `json:"count"`
	Pct       int    `json:"pct"`
	IsCorrect bool   `json:"isCorrect"`
}

// QuestionAnalysis is one question's full response breakdown.
type QuestionAnalysis struct {
	Index        int          `json:"index"`
	Text         string       `json:"text"`
	MediaURL     string       `json:"mediaUrl,omitempty"`
	MediaKind    string       `json:"mediaKind,omitempty"`
	TimeLimitSec int          `json:"timeLimitSec"`
	Points       int          `json:"points"`
	Options      []OptionStat `json:"options"`
	Answered     int          `json:"answered"`
	Correct      int          `json:"correct"`
	NoAnswer     int          `json:"noAnswer"`
	AvgMs        int64        `json:"avgMs"`
}

// AnswerDetail is one participant's response to one question.
type AnswerDetail struct {
	QuestionIndex int    `json:"questionIndex"`
	Selected      int    `json:"selected"` // -1 = no answer
	SelectedText  string `json:"selectedText"`
	Correct       bool   `json:"correct"`
	Points        int    `json:"points"`
	ElapsedMs     int64  `json:"elapsedMs"`
}

// ParticipantAnalysis is one candidate's full record for this session.
type ParticipantAnalysis struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Avatar     string         `json:"avatar"`
	Rank       int            `json:"rank"`
	Score      int            `json:"score"`
	Correct    int            `json:"correct"`
	Answered   int            `json:"answered"`
	AccuracyPc int            `json:"accuracyPc"`
	AvgMs      int64          `json:"avgMs"`
	Device     string         `json:"device"`
	JoinedAt   string         `json:"joinedAt"`
	Status     string         `json:"status"`
	Answers    []AnswerDetail `json:"answers"`
}

// Analytics is the full interactive report for one session.
type Analytics struct {
	Session      SessionMeta            `json:"session"`
	Stats        Stats                  `json:"stats"`
	Standings    []model.Standing       `json:"standings"`
	Questions    []QuestionAnalysis     `json:"questions"`
	Participants []ParticipantAnalysis  `json:"participants"`
}

// BuildAnalytics computes the full interactive report for a session.
func BuildAnalytics(quiz *model.Quiz, sess *model.Session) *Analytics {
	idx := answerIndex(sess)
	total := len(quiz.Questions)

	questions := make([]QuestionAnalysis, 0, total)
	answeredTotal, correctTotal := 0, 0
	for qi, q := range quiz.Questions {
		counts := make([]int, len(q.Options))
		answered, correct := 0, 0
		var totalMs int64
		for _, a := range sess.Answers {
			if a.QuestionIndex != qi {
				continue
			}
			answered++
			totalMs += a.ElapsedMs
			if a.OptionIndex >= 0 && a.OptionIndex < len(counts) {
				counts[a.OptionIndex]++
			}
			if a.Correct {
				correct++
			}
		}
		answeredTotal += answered
		correctTotal += correct

		opts := make([]OptionStat, 0, len(q.Options))
		for oi, o := range q.Options {
			pct := 0
			if answered > 0 {
				pct = int(float64(counts[oi])/float64(answered)*100 + 0.5)
			}
			mediaURL := ""
			if o.Media != nil {
				mediaURL = o.Media.URL()
			}
			opts = append(opts, OptionStat{
				Label: string(rune('A' + oi)), Text: o.Text, MediaURL: mediaURL,
				Count: counts[oi], Pct: pct, IsCorrect: oi == q.CorrectIndex,
			})
		}

		qMediaURL, qMediaKind := "", ""
		if q.Media != nil {
			qMediaURL, qMediaKind = q.Media.URL(), string(q.Media.Kind)
		}
		questions = append(questions, QuestionAnalysis{
			Index: qi, Text: q.Text, MediaURL: qMediaURL, MediaKind: qMediaKind,
			TimeLimitSec: q.TimeLimitSec, Points: q.Points, Options: opts,
			Answered: answered, Correct: correct,
			NoAnswer: len(sess.Participants) - answered, AvgMs: safeAvg(totalMs, answered),
		})
	}

	accuracy := 0
	if answeredTotal > 0 {
		accuracy = int(float64(correctTotal)/float64(answeredTotal)*100 + 0.5)
	}

	rankByID := map[string]int{}
	standings := session.Standings(sess)
	for _, s := range standings {
		rankByID[s.ID] = s.Rank
	}

	participants := make([]ParticipantAnalysis, 0, len(sess.Participants))
	for _, p := range sortedParticipants(sess) {
		answers := make([]AnswerDetail, 0, total)
		for qi, q := range quiz.Questions {
			a, ok := idx[p.ID][qi]
			if !ok {
				answers = append(answers, AnswerDetail{QuestionIndex: qi, Selected: -1, SelectedText: optionLabel(q, -1)})
				continue
			}
			answers = append(answers, AnswerDetail{
				QuestionIndex: qi, Selected: a.OptionIndex, SelectedText: optionLabel(q, a.OptionIndex),
				Correct: a.Correct, Points: a.Points, ElapsedMs: a.ElapsedMs,
			})
		}
		accuracyPc := 0
		if p.Answered > 0 {
			accuracyPc = int(float64(p.Correct)/float64(p.Answered)*100 + 0.5)
		}
		participants = append(participants, ParticipantAnalysis{
			ID: p.ID, Name: p.Name, Avatar: p.Avatar, Rank: rankByID[p.ID],
			Score: p.Score, Correct: p.Correct, Answered: p.Answered, AccuracyPc: accuracyPc,
			AvgMs: safeAvg(p.TotalMs, p.Answered), Device: p.Device,
			JoinedAt: p.JoinedAt.Format(timeLayout), Status: completion(sess, total, p),
			Answers: answers,
		})
	}

	dur := Duration(sess)
	meta := SessionMeta{
		Code: sess.Code, QuizID: sess.QuizID, Title: quiz.Title, Description: quiz.Description,
		Phase: string(sess.Phase), CreatedAt: sess.CreatedAt.Format(timeLayout),
		ExpiresAt: sess.ExpiresAt.Format(timeLayout),
	}
	if !sess.StartedAt.IsZero() {
		meta.StartedAt = sess.StartedAt.Format(timeLayout)
	}
	if !sess.EndedAt.IsZero() {
		meta.EndedAt = sess.EndedAt.Format(timeLayout)
	}

	return &Analytics{
		Session: meta,
		Stats: Stats{
			Participants: len(sess.Participants), Questions: total, Responses: len(sess.Answers),
			AccuracyPc: accuracy, DurationSec: int64(dur.Seconds()), Duration: formatDuration(dur),
		},
		Standings: standings, Questions: questions, Participants: participants,
	}
}
