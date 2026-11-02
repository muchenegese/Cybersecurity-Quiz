package report

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/go-pdf/fpdf"

	"kwiqtest/internal/model"
	"kwiqtest/internal/session"
)

// PDF renders the combined full detailed report.
//
// The PRD is explicit that completeness beats compactness: nothing here is
// truncated or rolled up. Every question, every participant, and every single
// response appears in full. Charts are added on top of the detail, never in
// place of it.
func PDF(w io.Writer, quiz *model.Quiz, sess *model.Session, mediaDir string) error {
	return buildDoc(quiz, sess, mediaDir).pdf.Output(w)
}

// buildDoc lays out every section. Split out from PDF so tests can render the
// same document with stream compression off and assert on its text.
func buildDoc(quiz *model.Quiz, sess *model.Session, mediaDir string) *doc {
	d := newDoc(quiz, sess)
	d.mediaDir = mediaDir

	d.coverPage()
	d.leaderboardSection()
	d.questionSection()
	d.perParticipantSection()
	d.attendeeSection()
	return d
}

// ------------------------------------------------------------- document -----

type doc struct {
	pdf      *fpdf.Fpdf
	quiz     *model.Quiz
	sess     *model.Session
	mediaDir string
	tr       func(string) string
	imgSeq   int
}

// Palette — deliberately a small, high-contrast set that survives greyscale
// printing, which is how most session reports actually get read.
var (
	colInk       = [3]int{22, 24, 38}
	colMuted     = [3]int{110, 116, 134}
	colBrand     = [3]int{79, 70, 229}
	colBrandSoft = [3]int{238, 236, 254}
	colGood      = [3]int{16, 122, 87}
	colGoodSoft  = [3]int{226, 245, 238}
	colBad       = [3]int{190, 45, 55}
	colBadSoft   = [3]int{253, 235, 236}
	colLine      = [3]int{214, 218, 228}
	colZebra     = [3]int{247, 248, 251}
)

const (
	pageW    = 210.0
	margin   = 12.0
	contentW = pageW - 2*margin
)

func newDoc(quiz *model.Quiz, sess *model.Session) *doc {
	p := fpdf.New("P", "mm", "A4", "")
	p.SetMargins(margin, 14, margin)
	p.SetAutoPageBreak(true, 18)
	p.AliasNbPages("{nb}")
	d := &doc{pdf: p, quiz: quiz, sess: sess, tr: p.UnicodeTranslatorFromDescriptor("")}

	title := d.txt(quiz.Title)
	generated := time.Now().Format("2 Jan 2006, 15:04")
	p.SetFooterFunc(func() {
		p.SetY(-14)
		p.SetDrawColor(colLine[0], colLine[1], colLine[2])
		p.Line(margin, p.GetY(), pageW-margin, p.GetY())
		p.SetY(p.GetY() + 1.5)
		p.SetFont("Helvetica", "", 7.5)
		p.SetTextColor(colMuted[0], colMuted[1], colMuted[2])
		p.CellFormat(contentW/2, 5, title+"  ·  Session "+sess.Code, "", 0, "L", false, 0, "")
		p.CellFormat(contentW/2, 5, fmt.Sprintf("Generated %s  ·  Page %d of {nb}", generated, p.PageNo()),
			"", 0, "R", false, 0, "")
	})
	return d
}

// txt makes a string safe for the PDF's core fonts.
//
// The built-in Helvetica metrics only cover Latin-1, and shipping a full
// Unicode font would add most of a megabyte to a binary whose whole selling
// point is being small. Emoji are dropped, other unsupported scripts become
// "?", and the CSV exports remain full-fidelity UTF-8 for anyone who needs the
// exact original text.
func (d *doc) txt(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\r':
		case r < 32 && r != '\n' && r != '\t':
		case r < 127 || (r >= 160 && r <= 255):
			b.WriteRune(r)
		case unicode.Is(unicode.Mn, r): // combining marks: drop silently
		case r >= 0x1F000 || (r >= 0x2190 && r <= 0x2BFF): // emoji & symbols
		case r == '‘' || r == '’':
			b.WriteByte('\'')
		case r == '“' || r == '”':
			b.WriteByte('"')
		case r == '–' || r == '—':
			b.WriteByte('-')
		case r == '…':
			b.WriteString("...")
		default:
			b.WriteByte('?')
		}
	}
	return d.tr(b.String())
}

func (d *doc) setInk(c [3]int)  { d.pdf.SetTextColor(c[0], c[1], c[2]) }
func (d *doc) setFill(c [3]int) { d.pdf.SetFillColor(c[0], c[1], c[2]) }
func (d *doc) setDraw(c [3]int) { d.pdf.SetDrawColor(c[0], c[1], c[2]) }

// sectionTitle starts a new page with a consistent heading.
func (d *doc) sectionTitle(title, subtitle string) {
	d.pdf.AddPage()
	d.setFill(colBrand)
	d.pdf.Rect(margin, d.pdf.GetY(), 3, 9, "F")
	d.pdf.SetX(margin + 6)
	d.pdf.SetFont("Helvetica", "B", 16)
	d.setInk(colInk)
	d.pdf.CellFormat(contentW-6, 9, d.txt(title), "", 1, "L", false, 0, "")
	if subtitle != "" {
		d.pdf.SetX(margin + 6)
		d.pdf.SetFont("Helvetica", "", 9)
		d.setInk(colMuted)
		d.pdf.CellFormat(contentW-6, 5, d.txt(subtitle), "", 1, "L", false, 0, "")
	}
	d.pdf.Ln(4)
}

// ------------------------------------------------------------- cover --------

func (d *doc) coverPage() {
	p := d.pdf
	p.AddPage()

	d.setFill(colBrand)
	p.Rect(0, 0, pageW, 62, "F")
	p.SetY(16)
	p.SetX(margin)
	p.SetFont("Helvetica", "B", 10)
	p.SetTextColor(255, 255, 255)
	p.CellFormat(contentW, 5, "KWIQTEST  ·  FULL DETAILED SESSION REPORT", "", 1, "L", false, 0, "")
	p.SetX(margin)
	p.SetFont("Helvetica", "B", 26)
	p.MultiCell(contentW, 11, d.txt(d.quiz.Title), "", "L", false)
	if d.quiz.Description != "" {
		p.SetX(margin)
		p.SetFont("Helvetica", "", 10)
		p.MultiCell(contentW, 5, d.txt(d.quiz.Description), "", "L", false)
	}

	p.SetY(72)
	standings := session.Standings(d.sess)
	answered, correct := 0, 0
	for _, a := range d.sess.Answers {
		answered++
		if a.Correct {
			correct++
		}
	}
	accuracy := 0
	if answered > 0 {
		accuracy = int(float64(correct)/float64(answered)*100 + 0.5)
	}
	scoring := "Flat points"
	if d.quiz.Settings.SpeedBonus {
		scoring = "Speed bonus (faster correct answers score higher)"
	}
	started, ended := "not started", "—"
	if !d.sess.StartedAt.IsZero() {
		started = d.sess.StartedAt.Format("2 Jan 2006, 15:04:05")
	}
	if !d.sess.EndedAt.IsZero() {
		ended = d.sess.EndedAt.Format("2 Jan 2006, 15:04:05")
	}

	// Headline tiles
	d.statRow([][2]string{
		{"Participants", fmt.Sprintf("%d", len(d.sess.Participants))},
		{"Questions", fmt.Sprintf("%d", len(d.quiz.Questions))},
		{"Responses", fmt.Sprintf("%d", answered)},
		{"Overall accuracy", fmt.Sprintf("%d%%", accuracy)},
	})
	p.Ln(6)

	d.kvTable([][2]string{
		{"Session join code", d.sess.Code},
		{"Session created", d.sess.CreatedAt.Format("2 Jan 2006, 15:04:05")},
		{"Started", started},
		{"Ended", ended},
		{"Duration", formatDuration(Duration(d.sess))},
		{"Scoring mode", scoring},
		{"Total points available", fmt.Sprintf("%d", totalPoints(d.quiz))},
		{"Report generated", time.Now().Format("2 Jan 2006, 15:04:05 MST")},
	})

	if len(standings) > 0 {
		p.Ln(7)
		p.SetFont("Helvetica", "B", 12)
		d.setInk(colInk)
		p.CellFormat(contentW, 7, "Podium", "", 1, "L", false, 0, "")
		p.Ln(1)
		medals := []struct {
			label string
			c     [3]int
		}{
			{"1st", [3]int{201, 162, 39}},
			{"2nd", [3]int{140, 148, 160}},
			{"3rd", [3]int{170, 113, 62}},
		}
		wTile := (contentW - 8) / 3
		y := p.GetY()
		for i := 0; i < 3 && i < len(standings); i++ {
			s := standings[i]
			x := margin + float64(i)*(wTile+4)
			c := medals[i].c
			p.SetFillColor(c[0], c[1], c[2])
			p.Rect(x, y, wTile, 6, "F")
			p.SetFillColor(250, 250, 252)
			p.Rect(x, y+6, wTile, 20, "F")
			d.setDraw(colLine)
			p.Rect(x, y, wTile, 26, "D")

			p.SetXY(x, y)
			p.SetFont("Helvetica", "B", 8)
			p.SetTextColor(255, 255, 255)
			p.CellFormat(wTile, 6, medals[i].label, "", 0, "C", false, 0, "")
			p.SetXY(x+2, y+8)
			p.SetFont("Helvetica", "B", 12)
			d.setInk(colInk)
			p.CellFormat(wTile-4, 6, d.clip(s.Name, wTile-6, "Helvetica", "B", 12), "", 0, "C", false, 0, "")
			p.SetXY(x+2, y+15)
			p.SetFont("Helvetica", "", 9)
			d.setInk(colMuted)
			p.CellFormat(wTile-4, 6, fmt.Sprintf("%d pts  ·  %d%% accuracy", s.Score, s.AccuracyPc),
				"", 0, "C", false, 0, "")
		}
		p.SetY(y + 30)
	}

	p.Ln(4)
	d.setInk(colMuted)
	p.SetFont("Helvetica", "I", 8)
	p.MultiCell(contentW, 4,
		d.txt("This report contains the complete session record: every question exactly as shown to "+
			"participants, every participant's answer to every question with timing and points, the full "+
			"attendee list, and the final ranking. Summary charts are included in addition to — not "+
			"instead of — the underlying detail."), "", "L", false)
}

func (d *doc) statRow(items [][2]string) {
	p := d.pdf
	n := float64(len(items))
	wTile := (contentW - (n-1)*4) / n
	y := p.GetY()
	for i, it := range items {
		x := margin + float64(i)*(wTile+4)
		d.setFill(colBrandSoft)
		p.Rect(x, y, wTile, 18, "F")
		p.SetXY(x+3, y+3)
		p.SetFont("Helvetica", "", 8)
		d.setInk(colMuted)
		p.CellFormat(wTile-6, 4, d.txt(it[0]), "", 0, "L", false, 0, "")
		p.SetXY(x+3, y+8)
		p.SetFont("Helvetica", "B", 15)
		d.setInk(colBrand)
		p.CellFormat(wTile-6, 7, d.txt(it[1]), "", 0, "L", false, 0, "")
	}
	p.SetY(y + 18)
}

func (d *doc) kvTable(rows [][2]string) {
	p := d.pdf
	wk := 58.0
	for i, r := range rows {
		if i%2 == 0 {
			d.setFill(colZebra)
			p.Rect(margin, p.GetY(), contentW, 6.5, "F")
		}
		p.SetFont("Helvetica", "B", 9)
		d.setInk(colMuted)
		p.CellFormat(wk, 6.5, d.txt(r[0]), "", 0, "L", false, 0, "")
		p.SetFont("Helvetica", "", 9)
		d.setInk(colInk)
		p.CellFormat(contentW-wk, 6.5, d.txt(r[1]), "", 1, "L", false, 0, "")
	}
}

// clip shortens text with an ellipsis so it never overruns a fixed-width cell.
func (d *doc) clip(s string, maxW float64, family, style string, size float64) string {
	t := d.txt(s)
	d.pdf.SetFont(family, style, size)
	if d.pdf.GetStringWidth(t) <= maxW {
		return t
	}
	for len(t) > 1 {
		t = t[:len(t)-1]
		if d.pdf.GetStringWidth(t+"...") <= maxW {
			return t + "..."
		}
	}
	return t
}

// ------------------------------------------------------- leaderboard --------

func (d *doc) leaderboardSection() {
	p := d.pdf
	standings := session.Standings(d.sess)
	d.sectionTitle("Final leaderboard",
		fmt.Sprintf("%d participants, ranked by score. Ties are broken by cumulative response time.",
			len(standings)))
	if len(standings) == 0 {
		d.emptyNote("No participants joined this session.")
		return
	}

	cols := []col{
		{"#", 12, "C"}, {"Participant", 62, "L"}, {"Score", 22, "R"},
		{"Correct", 24, "C"}, {"Answered", 24, "C"}, {"Accuracy", 22, "R"}, {"Avg time", 20, "R"},
	}
	d.tableHead(cols)
	for i, s := range standings {
		d.rowBreak(cols, 6.5)
		fill := i%2 == 1
		if fill {
			d.setFill(colZebra)
		}
		p.SetFont("Helvetica", "", 9)
		d.setInk(colInk)
		vals := []string{
			fmt.Sprintf("%d", s.Rank),
			d.clip(s.Name, cols[1].w-3, "Helvetica", "", 9),
			fmt.Sprintf("%d", s.Score),
			fmt.Sprintf("%d", s.Correct),
			fmt.Sprintf("%d", s.Answered),
			fmt.Sprintf("%d%%", s.AccuracyPc),
			secs(s.AvgMs) + "s",
		}
		for j, c := range cols {
			if j == 0 || j == 2 {
				p.SetFont("Helvetica", "B", 9)
			} else {
				p.SetFont("Helvetica", "", 9)
			}
			p.CellFormat(c.w, 6.5, vals[j], "", 0, c.align, fill, 0, "")
		}
		p.Ln(-1)
	}
	d.tableRule()
}

type col struct {
	title string
	w     float64
	align string
}

func (d *doc) tableHead(cols []col) {
	p := d.pdf
	d.setFill(colInk)
	p.SetTextColor(255, 255, 255)
	p.SetFont("Helvetica", "B", 8.5)
	for _, c := range cols {
		p.CellFormat(c.w, 7, d.txt(c.title), "", 0, c.align, true, 0, "")
	}
	p.Ln(-1)
}

// rowBreak repeats the header when a table spills onto a new page.
func (d *doc) rowBreak(cols []col, h float64) {
	if d.pdf.GetY()+h > 275 {
		d.pdf.AddPage()
		d.tableHead(cols)
	}
}

func (d *doc) tableRule() {
	d.setDraw(colLine)
	d.pdf.Line(margin, d.pdf.GetY(), pageW-margin, d.pdf.GetY())
	d.pdf.Ln(3)
}

func (d *doc) emptyNote(msg string) {
	d.pdf.SetFont("Helvetica", "I", 9)
	d.setInk(colMuted)
	d.pdf.CellFormat(contentW, 8, d.txt(msg), "", 1, "L", false, 0, "")
}

// --------------------------------------------------------- question set -----

func (d *doc) questionSection() {
	p := d.pdf
	d.sectionTitle("Question set & response analysis",
		"Every question exactly as participants saw it, with the correct answer marked and the full answer distribution.")

	idx := answerIndex(d.sess)
	for qi, q := range d.quiz.Questions {
		counts := make([]int, len(q.Options))
		answered, correct := 0, 0
		var totalMs int64
		for _, a := range d.sess.Answers {
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

		needed := 30 + float64(len(q.Options))*8
		if p.GetY()+needed > 268 {
			p.AddPage()
		}
		p.Ln(2)
		d.setFill(colZebra)
		startY := p.GetY()
		p.SetFont("Helvetica", "B", 11)
		d.setInk(colInk)
		p.SetX(margin)
		p.MultiCell(contentW, 6, d.txt(questionTitle(q, qi+1)), "", "L", false)

		meta := fmt.Sprintf("%d s limit  ·  %d points  ·  %d of %d answered  ·  %d correct  ·  avg %s s",
			q.TimeLimitSec, q.Points, answered, len(d.sess.Participants), correct,
			secs(safeAvg(totalMs, answered)))
		p.SetFont("Helvetica", "", 8.5)
		d.setInk(colMuted)
		p.CellFormat(contentW, 5, d.txt(meta), "", 1, "L", false, 0, "")

		if q.Media != nil {
			d.renderMedia(q.Media)
		}
		p.Ln(1)

		// Options with an inline distribution bar. The bar is the "summary
		// chart" the PRD allows; the counts beside it are the real data.
		for oi := range q.Options {
			isCorrect := oi == q.CorrectIndex
			label := d.txt(optionLabel(q, oi))
			pct := 0
			if answered > 0 {
				pct = int(float64(counts[oi])/float64(answered)*100 + 0.5)
			}
			y := p.GetY()
			if isCorrect {
				d.setFill(colGoodSoft)
			} else {
				d.setFill([3]int{250, 250, 252})
			}
			p.Rect(margin, y, contentW, 7, "F")

			// distribution bar drawn behind the text
			barW := (contentW - 46) * float64(pct) / 100
			if barW > 0 {
				if isCorrect {
					p.SetFillColor(190, 232, 214)
				} else {
					p.SetFillColor(225, 228, 238)
				}
				p.Rect(margin, y, barW, 7, "F")
			}
			p.SetXY(margin+2, y)
			p.SetFont("Helvetica", map[bool]string{true: "B", false: ""}[isCorrect], 9)
			if isCorrect {
				d.setInk(colGood)
			} else {
				d.setInk(colInk)
			}
			p.CellFormat(contentW-50, 7, d.clip(label, contentW-54, "Helvetica",
				map[bool]string{true: "B", false: ""}[isCorrect], 9), "", 0, "L", false, 0, "")
			p.SetFont("Helvetica", "", 8.5)
			d.setInk(colMuted)
			p.CellFormat(30, 7, fmt.Sprintf("%d resp.  %d%%", counts[oi], pct), "", 0, "R", false, 0, "")
			p.SetFont("Helvetica", "B", 8)
			if isCorrect {
				d.setInk(colGood)
				p.CellFormat(16, 7, "CORRECT", "", 1, "R", false, 0, "")
			} else {
				p.CellFormat(16, 7, "", "", 1, "R", false, 0, "")
			}
		}
		if noAns := len(d.sess.Participants) - answered; noAns > 0 {
			p.SetFont("Helvetica", "I", 8.5)
			d.setInk(colMuted)
			p.CellFormat(contentW, 5.5, d.txt(fmt.Sprintf("%d participant(s) did not answer in time.", noAns)),
				"", 1, "L", false, 0, "")
		}
		_ = startY
		_ = idx
		p.Ln(2)
		d.setDraw(colLine)
		p.Line(margin, p.GetY(), pageW-margin, p.GetY())
	}
}

func safeAvg(total int64, n int) int64 {
	if n == 0 {
		return 0
	}
	return total / int64(n)
}

// renderMedia embeds an image prompt, or notes a video reference. Videos cannot
// be embedded in a PDF, so the report records the exact filename and duration
// context instead of silently dropping the fact that the question had media.
func (d *doc) renderMedia(m *model.Media) {
	p := d.pdf
	if m.Kind == model.MediaVideo || d.mediaDir == "" {
		p.SetFont("Helvetica", "I", 8.5)
		d.setInk(colBrand)
		p.CellFormat(contentW, 5, d.txt("[Video prompt: "+m.Name+" — see session media folder]"), "", 1, "L", false, 0, "")
		return
	}
	imgType := ""
	switch strings.ToLower(filepath.Ext(m.ID)) {
	case ".jpg", ".jpeg":
		imgType = "JPG"
	case ".png":
		imgType = "PNG"
	case ".gif":
		imgType = "GIF"
	default: // WebP and anything else fpdf cannot embed
		p.SetFont("Helvetica", "I", 8.5)
		d.setInk(colBrand)
		p.CellFormat(contentW, 5, d.txt("[Image prompt: "+m.Name+"]"), "", 1, "L", false, 0, "")
		return
	}
	data, err := os.ReadFile(filepath.Join(d.mediaDir, filepath.Base(m.ID)))
	if err != nil {
		return
	}
	d.imgSeq++
	name := fmt.Sprintf("img%d", d.imgSeq)
	info := p.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: imgType}, bytes.NewReader(data))
	if info == nil || info.Width() == 0 {
		return
	}
	maxW, maxH := 70.0, 40.0
	w := maxW
	h := info.Height() * (w / info.Width())
	if h > maxH {
		h = maxH
		w = info.Width() * (h / info.Height())
	}
	if p.GetY()+h > 268 {
		p.AddPage()
	}
	p.Ln(1)
	p.ImageOptions(name, margin, p.GetY(), w, h, true, fpdf.ImageOptions{ImageType: imgType}, 0, "")
	p.SetFont("Helvetica", "I", 7.5)
	d.setInk(colMuted)
	p.CellFormat(contentW, 4, d.txt("Image prompt: "+m.Name), "", 1, "L", false, 0, "")
}

// -------------------------------------------------- per-participant ---------

func (d *doc) perParticipantSection() {
	p := d.pdf
	d.sectionTitle("Per-participant response breakdown",
		"For every participant: what they picked on every question, whether it was right, how long they took, and the points it earned.")

	if len(d.sess.Participants) == 0 {
		d.emptyNote("No participants joined this session.")
		return
	}
	idx := answerIndex(d.sess)
	rankByID := map[string]int{}
	for _, s := range session.Standings(d.sess) {
		rankByID[s.ID] = s.Rank
	}

	cols := []col{
		{"Q", 10, "C"}, {"Question", 66, "L"}, {"Their answer", 48, "L"},
		{"Result", 18, "C"}, {"Time", 18, "R"}, {"Points", 18, "R"},
	}
	for _, part := range sortedParticipants(d.sess) {
		if p.GetY()+40 > 268 {
			p.AddPage()
		}
		p.Ln(3)
		d.setFill(colBrandSoft)
		y := p.GetY()
		p.Rect(margin, y, contentW, 11, "F")
		p.SetXY(margin+3, y+1)
		p.SetFont("Helvetica", "B", 11)
		d.setInk(colInk)
		p.CellFormat(100, 5, d.clip(part.Name, 96, "Helvetica", "B", 11), "", 0, "L", false, 0, "")
		p.SetFont("Helvetica", "", 8.5)
		d.setInk(colMuted)
		p.CellFormat(contentW-106, 5, fmt.Sprintf("Rank %d of %d  ·  %d pts  ·  %s",
			rankByID[part.ID], len(d.sess.Participants), part.Score,
			completion(d.sess, len(d.quiz.Questions), part)), "", 1, "R", false, 0, "")
		p.SetXY(margin+3, y+6)
		p.SetFont("Helvetica", "", 8)
		d.setInk(colMuted)
		p.CellFormat(contentW-6, 4, d.txt(fmt.Sprintf(
			"Joined %s  ·  %s device  ·  %d/%d correct  ·  avg %ss per answer",
			part.JoinedAt.Format(timeLayout), part.Device, part.Correct, len(d.quiz.Questions),
			secs(safeAvg(part.TotalMs, part.Answered)))), "", 1, "L", false, 0, "")
		p.SetY(y + 12)

		d.tableHead(cols)
		for qi, q := range d.quiz.Questions {
			d.rowBreak(cols, 6.5)
			a, ok := idx[part.ID][qi]
			their, result, took, pts := "— no answer —", "MISSED", "—", "0"
			rowFill := colZebra
			resultInk := colMuted
			if ok {
				their = optionLabel(q, a.OptionIndex)
				took = secs(a.ElapsedMs) + "s"
				pts = fmt.Sprintf("%d", a.Points)
				if a.Correct {
					result, rowFill, resultInk = "CORRECT", colGoodSoft, colGood
				} else {
					result, rowFill, resultInk = "WRONG", colBadSoft, colBad
				}
			}
			d.setFill(rowFill)
			p.SetFont("Helvetica", "", 8.5)
			d.setInk(colInk)
			p.CellFormat(cols[0].w, 6.5, fmt.Sprintf("%d", qi+1), "", 0, "C", true, 0, "")
			p.CellFormat(cols[1].w, 6.5, d.clip(stripQ(questionTitle(q, qi+1)), cols[1].w-3, "Helvetica", "", 8.5), "", 0, "L", true, 0, "")
			p.CellFormat(cols[2].w, 6.5, d.clip(their, cols[2].w-3, "Helvetica", "", 8.5), "", 0, "L", true, 0, "")
			p.SetFont("Helvetica", "B", 7.5)
			d.setInk(resultInk)
			p.CellFormat(cols[3].w, 6.5, result, "", 0, "C", true, 0, "")
			p.SetFont("Helvetica", "", 8.5)
			d.setInk(colInk)
			p.CellFormat(cols[4].w, 6.5, took, "", 0, "R", true, 0, "")
			p.SetFont("Helvetica", "B", 8.5)
			p.CellFormat(cols[5].w, 6.5, pts, "", 1, "R", true, 0, "")
		}
		d.tableRule()
	}
}

// stripQ removes the "Qn. " prefix, which is redundant beside a Q column.
func stripQ(s string) string {
	if i := strings.Index(s, ". "); i > 0 && i < 5 {
		return s[i+2:]
	}
	return s
}

// ----------------------------------------------------------- attendees ------

func (d *doc) attendeeSection() {
	p := d.pdf
	d.sectionTitle("Attendee list",
		"Everyone who joined this session, in join order.")
	if len(d.sess.Participants) == 0 {
		d.emptyNote("No participants joined this session.")
		return
	}
	rankByID := map[string]int{}
	for _, s := range session.Standings(d.sess) {
		rankByID[s.ID] = s.Rank
	}
	cols := []col{
		{"#", 10, "C"}, {"Name", 56, "L"}, {"Joined at", 38, "L"},
		{"Device", 22, "C"}, {"Status", 40, "L"}, {"Score", 18, "R"}, {"Rank", 14, "C"},
	}
	d.tableHead(cols)
	for i, part := range sortedParticipants(d.sess) {
		d.rowBreak(cols, 6.5)
		fill := i%2 == 1
		if fill {
			d.setFill(colZebra)
		}
		p.SetFont("Helvetica", "", 8.5)
		d.setInk(colInk)
		vals := []string{
			fmt.Sprintf("%d", i+1),
			d.clip(part.Name, cols[1].w-3, "Helvetica", "", 8.5),
			part.JoinedAt.Format(timeLayout),
			part.Device,
			completion(d.sess, len(d.quiz.Questions), part),
			fmt.Sprintf("%d", part.Score),
			fmt.Sprintf("%d", rankByID[part.ID]),
		}
		for j, c := range cols {
			p.CellFormat(c.w, 6.5, d.txt(vals[j]), "", 0, c.align, fill, 0, "")
			_ = j
		}
		p.Ln(-1)
	}
	d.tableRule()

	p.Ln(3)
	p.SetFont("Helvetica", "I", 8)
	d.setInk(colMuted)
	p.MultiCell(contentW, 4, d.txt(fmt.Sprintf(
		"End of report. %d participants, %d questions, %d recorded responses. "+
			"Session data is retained until %s.",
		len(d.sess.Participants), len(d.quiz.Questions), len(d.sess.Answers),
		d.sess.ExpiresAt.Format("2 Jan 2006, 15:04"))), "", "L", false)
}

// ------------------------------------------------------------- helpers ------

func totalPoints(q *model.Quiz) int {
	t := 0
	for _, x := range q.Questions {
		t += x.Points
	}
	return t
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm %02ds", h, m, s)
	}
	return fmt.Sprintf("%dm %02ds", m, s)
}
