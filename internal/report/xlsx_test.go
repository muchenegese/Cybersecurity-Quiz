package report

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"

	"kwiqtest/internal/model"
)

func buildXLSX(t *testing.T) map[string]string {
	t.Helper()
	quiz, sess := fixture()
	var buf bytes.Buffer
	if err := Excel(&buf, quiz, sess); err != nil {
		t.Fatalf("Excel: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("produced file is not a valid zip: %v", err)
	}
	parts := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		parts[f.Name] = string(b)
	}
	return parts
}

func TestExcelProducesValidWorkbookStructure(t *testing.T) {
	parts := buildXLSX(t)
	for _, want := range []string{
		"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml",
		"xl/_rels/workbook.xml.rels", "xl/styles.xml",
		"xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml",
		"xl/worksheets/sheet3.xml", "xl/worksheets/sheet4.xml",
	} {
		if _, ok := parts[want]; !ok {
			t.Errorf("workbook missing required part %q", want)
		}
	}
	wb := parts["xl/workbook.xml"]
	for _, name := range []string{"Leaderboard", "Questions", "Attendees", "Responses"} {
		if !strings.Contains(wb, `name="`+name+`"`) {
			t.Errorf("workbook.xml missing sheet %q: %s", name, wb)
		}
	}
}

func TestExcelSheetsContainExpectedData(t *testing.T) {
	parts := buildXLSX(t)

	leaderboard := parts["xl/worksheets/sheet1.xml"]
	if !strings.Contains(leaderboard, "Ada Lovelace") {
		t.Error("Leaderboard sheet missing top scorer")
	}
	if !strings.Contains(leaderboard, `<c r="A1" s="1"`) {
		t.Error("Leaderboard header row should use the bold header style (s=\"1\")")
	}

	questions := parts["xl/worksheets/sheet2.xml"]
	if !strings.Contains(questions, "Capital of France?") {
		t.Error("Questions sheet missing question text")
	}

	attendees := parts["xl/worksheets/sheet3.xml"]
	if !strings.Contains(attendees, "Silent Sam") {
		t.Error("Attendees sheet missing non-answering participant")
	}

	responses := parts["xl/worksheets/sheet4.xml"]
	if !strings.Contains(responses, "Grace Hopper") {
		t.Error("Responses sheet missing a recorded answer")
	}
	// Numeric cells (e.g. a score) must not be quoted as inline strings.
	if !strings.Contains(leaderboard, "<v>1000</v>") && !strings.Contains(leaderboard, "<v>3000</v>") {
		t.Error("Leaderboard sheet has no plain numeric <v> cells for scores")
	}
}

func TestExcelHandlesEmptySession(t *testing.T) {
	quiz, _ := fixture()
	empty := &model.Session{Code: "000111", QuizID: quiz.ID}
	var buf bytes.Buffer
	if err := Excel(&buf, quiz, empty); err != nil {
		t.Fatalf("a session nobody joined should still export: %v", err)
	}
	if buf.Len() < 200 {
		t.Errorf("empty-session workbook suspiciously small: %d bytes", buf.Len())
	}
}

func TestColLetter(t *testing.T) {
	cases := map[int]string{0: "A", 1: "B", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA"}
	for in, want := range cases {
		if got := colLetter(in); got != want {
			t.Errorf("colLetter(%d) = %q, want %q", in, got, want)
		}
	}
}
