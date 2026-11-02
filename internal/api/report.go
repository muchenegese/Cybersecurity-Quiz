package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"kwiqtest/internal/model"
	"kwiqtest/internal/report"
)

// errNotOrganiser distinguishes "session doesn't exist" (404) from "it exists
// but you don't hold its host token" (403) — ownedSession's two failure modes.
var errNotOrganiser = errors.New("not the organiser")

var unsafeFilename = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// slug turns a quiz title into a safe download filename fragment.
func slug(s string) string {
	s = unsafeFilename.ReplaceAllString(strings.TrimSpace(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		s = "quiz"
	}
	return strings.ToLower(s)
}

// ownedSession loads a session snapshot and its quiz, verifying the caller
// holds the organiser's host token for it. Shared by report and analytics,
// which otherwise duplicate the exact same lookup-and-authorize sequence.
func (s *Server) ownedSession(r *http.Request, code string) (*model.Quiz, *model.Session, error) {
	sessCopy, err := s.st.SessionSnapshot(code)
	if err != nil {
		return nil, nil, err
	}
	if sessCopy.HostToken != hostToken(r) {
		return nil, nil, errNotOrganiser
	}
	quiz, err := s.st.Quiz(sessCopy.QuizID)
	if err != nil {
		return nil, nil, err
	}
	return quiz, sessCopy, nil
}

// report serves the post-session downloads. Everything is rendered server-side
// so output is identical regardless of the organiser's browser.
func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))

	quiz, sessCopy, err := s.ownedSession(r, code)
	if err != nil {
		if errors.Is(err, errNotOrganiser) {
			writeErr(w, http.StatusForbidden, "only the organiser can download results")
		} else {
			writeErr(w, http.StatusNotFound, "session not found")
		}
		return
	}
	quizCopy := *quiz

	base := fmt.Sprintf("kwiqtest-%s-%s", slug(quiz.Title), code)
	var buf bytes.Buffer
	var filename, contentType string

	switch r.URL.Query().Get("format") {
	case "", "pdf":
		filename, contentType = base+"-full-report.pdf", "application/pdf"
		err = report.PDF(&buf, &quizCopy, sessCopy, filepath.Join(s.st.Dir(), "media"))
	case "xlsx":
		filename, contentType = base+"-report.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		err = report.Excel(&buf, &quizCopy, sessCopy)
	case "leaderboard":
		filename, contentType = base+"-leaderboard.csv", "text/csv; charset=utf-8"
		err = report.LeaderboardCSV(&buf, &quizCopy, sessCopy)
	case "questions":
		filename, contentType = base+"-questions.csv", "text/csv; charset=utf-8"
		err = report.QuestionsCSV(&buf, &quizCopy, sessCopy)
	case "attendees":
		filename, contentType = base+"-attendees.csv", "text/csv; charset=utf-8"
		err = report.AttendeesCSV(&buf, &quizCopy, sessCopy)
	case "responses":
		filename, contentType = base+"-responses.csv", "text/csv; charset=utf-8"
		err = report.ResponsesCSV(&buf, &quizCopy, sessCopy)
	default:
		writeErr(w, http.StatusBadRequest,
			"unknown format — use pdf, xlsx, leaderboard, questions, attendees or responses")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not generate report: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// analytics serves the interactive dashboard's data: the same computed
// standings/question/participant breakdowns as the PDF and Excel exports,
// shaped as JSON for the frontend to render directly.
func (s *Server) analytics(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
	quiz, sessCopy, err := s.ownedSession(r, code)
	if err != nil {
		if errors.Is(err, errNotOrganiser) {
			writeErr(w, http.StatusForbidden, "only the organiser can view this session's analytics")
		} else {
			writeErr(w, http.StatusNotFound, "session not found")
		}
		return
	}
	writeJSON(w, http.StatusOK, report.BuildAnalytics(quiz, sessCopy))
}
