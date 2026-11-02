// Package report generates the post-quiz exports.
//
// Everything is produced server-side so the output is byte-identical no matter
// which browser or device the organiser downloads it from.
package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"kwiqtest/internal/model"
	"kwiqtest/internal/session"
)

const timeLayout = "2006-01-02 15:04:05"

// writeCSV emits a UTF-8 BOM before the data. Without it Excel on Windows
// decodes the file as the local codepage and mangles every non-ASCII name.
func writeCSV(w io.Writer, rows [][]string) error {
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.WriteAll(rows); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func secs(ms int64) string { return fmt.Sprintf("%.2f", float64(ms)/1000) }

// answerIndex builds a participant→question→answer lookup.
func answerIndex(sess *model.Session) map[string]map[int]model.Answer {
	idx := map[string]map[int]model.Answer{}
	for _, a := range sess.Answers {
		if idx[a.ParticipantID] == nil {
			idx[a.ParticipantID] = map[int]model.Answer{}
		}
		idx[a.ParticipantID][a.QuestionIndex] = a
	}
	return idx
}

// optionLabel renders an option as "A. Paris", falling back to a media note
// when the option is a picture rather than text.
func optionLabel(q model.Question, i int) string {
	if i < 0 || i >= len(q.Options) {
		return "— no answer —"
	}
	o := q.Options[i]
	text := o.Text
	if text == "" && o.Media != nil {
		text = "[" + string(o.Media.Kind) + ": " + o.Media.Name + "]"
	}
	return fmt.Sprintf("%c. %s", 'A'+i, text)
}

func questionTitle(q model.Question, n int) string {
	t := q.Text
	if t == "" && q.Media != nil {
		t = "[" + string(q.Media.Kind) + " question: " + q.Media.Name + "]"
	}
	return fmt.Sprintf("Q%d. %s", n, t)
}

// completion reports how far a participant got.
func completion(sess *model.Session, total int, p *model.Participant) string {
	switch {
	case total == 0:
		return "n/a"
	case p.Answered >= total:
		return "completed"
	case p.Answered == 0:
		return "joined, no answers"
	default:
		return fmt.Sprintf("partial (%d/%d)", p.Answered, total)
	}
}

// LeaderboardCSV writes the score summary: name, total score, rank, accuracy.
func LeaderboardCSV(w io.Writer, quiz *model.Quiz, sess *model.Session) error {
	return writeCSV(w, leaderboardRows(quiz, sess))
}

func leaderboardRows(quiz *model.Quiz, sess *model.Session) [][]string {
	rows := [][]string{{
		"Rank", "Name", "Total Score", "Correct", "Answered", "Total Questions",
		"Accuracy %", "Avg Response (s)", "Device", "Joined At",
	}}
	byID := map[string]*model.Participant{}
	for _, p := range sess.Participants {
		byID[p.ID] = p
	}
	for _, s := range session.Standings(sess) {
		p := byID[s.ID]
		joined, device := "", ""
		if p != nil {
			joined, device = p.JoinedAt.Format(timeLayout), p.Device
		}
		rows = append(rows, []string{
			strconv.Itoa(s.Rank), s.Name, strconv.Itoa(s.Score),
			strconv.Itoa(s.Correct), strconv.Itoa(s.Answered), strconv.Itoa(len(quiz.Questions)),
			strconv.Itoa(s.AccuracyPc), secs(s.AvgMs), device, joined,
		})
	}
	return rows
}

// QuestionsCSV writes the question-wise report: one row per question per
// participant, with the correct answer alongside what they picked.
func QuestionsCSV(w io.Writer, quiz *model.Quiz, sess *model.Session) error {
	return writeCSV(w, questionsRows(quiz, sess))
}

func questionsRows(quiz *model.Quiz, sess *model.Session) [][]string {
	rows := [][]string{{
		"Question #", "Question", "Media", "Correct Answer", "Points Available",
		"Time Limit (s)", "Participant", "Their Answer", "Correct?",
		"Time Taken (s)", "Points Earned",
	}}
	idx := answerIndex(sess)
	people := sortedParticipants(sess)
	for qi, q := range quiz.Questions {
		media := ""
		if q.Media != nil {
			media = string(q.Media.Kind) + ": " + q.Media.Name
		}
		for _, p := range people {
			a, answered := idx[p.ID][qi]
			their, correct, taken, earned := "— no answer —", "no", "", "0"
			if answered {
				their = optionLabel(q, a.OptionIndex)
				if a.Correct {
					correct = "yes"
				}
				taken = secs(a.ElapsedMs)
				earned = strconv.Itoa(a.Points)
			}
			rows = append(rows, []string{
				strconv.Itoa(qi + 1), q.Text, media,
				optionLabel(q, q.CorrectIndex), strconv.Itoa(q.Points),
				strconv.Itoa(q.TimeLimitSec), p.Name, their, correct, taken, earned,
			})
		}
	}
	return rows
}

// AttendeesCSV writes the attendee list.
func AttendeesCSV(w io.Writer, quiz *model.Quiz, sess *model.Session) error {
	return writeCSV(w, attendeesRows(quiz, sess))
}

func attendeesRows(quiz *model.Quiz, sess *model.Session) [][]string {
	rows := [][]string{{
		"Name", "Join Time", "Device", "Completion Status",
		"Questions Answered", "Total Questions", "Final Score", "Rank",
	}}
	rankByID := map[string]int{}
	for _, s := range session.Standings(sess) {
		rankByID[s.ID] = s.Rank
	}
	for _, p := range sortedParticipants(sess) {
		rows = append(rows, []string{
			p.Name, p.JoinedAt.Format(timeLayout), p.Device,
			completion(sess, len(quiz.Questions), p),
			strconv.Itoa(p.Answered), strconv.Itoa(len(quiz.Questions)),
			strconv.Itoa(p.Score), strconv.Itoa(rankByID[p.ID]),
		})
	}
	return rows
}

// ResponsesCSV writes the raw response log — one row per submitted answer, in
// the order they arrived. This is the audit trail.
func ResponsesCSV(w io.Writer, quiz *model.Quiz, sess *model.Session) error {
	return writeCSV(w, responsesRows(quiz, sess))
}

func responsesRows(quiz *model.Quiz, sess *model.Session) [][]string {
	rows := [][]string{{
		"Answered At", "Participant", "Question #", "Question",
		"Selected", "Correct Answer", "Correct?", "Time Taken (s)", "Points Earned",
	}}
	nameByID := map[string]string{}
	for _, p := range sess.Participants {
		nameByID[p.ID] = p.Name
	}
	answers := append([]model.Answer(nil), sess.Answers...)
	sort.SliceStable(answers, func(i, j int) bool { return answers[i].AnsweredAt.Before(answers[j].AnsweredAt) })
	for _, a := range answers {
		if a.QuestionIndex < 0 || a.QuestionIndex >= len(quiz.Questions) {
			continue
		}
		q := quiz.Questions[a.QuestionIndex]
		correct := "no"
		if a.Correct {
			correct = "yes"
		}
		rows = append(rows, []string{
			a.AnsweredAt.Format(timeLayout), nameByID[a.ParticipantID],
			strconv.Itoa(a.QuestionIndex + 1), q.Text,
			optionLabel(q, a.OptionIndex), optionLabel(q, q.CorrectIndex),
			correct, secs(a.ElapsedMs), strconv.Itoa(a.Points),
		})
	}
	return rows
}

// sortedParticipants orders attendees by join time for stable report output.
func sortedParticipants(sess *model.Session) []*model.Participant {
	out := append([]*model.Participant(nil), sess.Participants...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].JoinedAt.Before(out[j].JoinedAt) })
	return out
}

// Duration returns the wall-clock length of the session.
func Duration(sess *model.Session) time.Duration {
	if sess.StartedAt.IsZero() {
		return 0
	}
	end := sess.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(sess.StartedAt)
}
