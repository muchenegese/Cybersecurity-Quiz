package report

import (
	"archive/zip"
	"fmt"
	"io"
	"strconv"
	"strings"

	"kwiqtest/internal/model"
)

// Excel renders one workbook with a sheet per CSV export (Leaderboard,
// Questions, Attendees, Responses), so the same data that downloads as CSV
// today also opens directly in Excel/Sheets/Numbers with proper column types.
//
// Built by hand against the OOXML SpreadsheetML schema using only
// archive/zip and string templates, rather than pulling in a third-party
// library — this keeps the project's existing "pure Go, no transitive
// dependencies" stance (see go.mod) and needs no network access to build.
func Excel(w io.Writer, quiz *model.Quiz, sess *model.Session) error {
	sheets := []namedRows{
		{"Leaderboard", leaderboardRows(quiz, sess)},
		{"Questions", questionsRows(quiz, sess)},
		{"Attendees", attendeesRows(quiz, sess)},
		{"Responses", responsesRows(quiz, sess)},
	}
	return writeXLSX(w, sheets)
}

type namedRows struct {
	name string
	rows [][]string
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`

func writeXLSX(w io.Writer, sheets []namedRows) error {
	zw := zip.NewWriter(w)

	write := func(name, content string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, content)
		return err
	}

	steps := []struct{ name, content string }{
		{"[Content_Types].xml", contentTypesXML(len(sheets))},
		{"_rels/.rels", rootRelsXML()},
		{"xl/workbook.xml", workbookXML(sheets)},
		{"xl/_rels/workbook.xml.rels", workbookRelsXML(len(sheets))},
		{"xl/styles.xml", stylesXML()},
	}
	for _, s := range steps {
		if err := write(s.name, s.content); err != nil {
			return err
		}
	}
	for i, sh := range sheets {
		name := fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
		if err := write(name, sheetXML(sh.rows)); err != nil {
			return err
		}
	}
	return zw.Close()
}

func contentTypesXML(n int) string {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	b.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	b.WriteString(`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	b.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

func rootRelsXML() string {
	return xmlHeader + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
		`</Relationships>`
}

func workbookXML(sheets []namedRows) string {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, sh := range sheets {
		fmt.Fprintf(&b, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, xmlEscape(sh.name), i+1, i+1)
	}
	b.WriteString(`</sheets></workbook>`)
	return b.String()
}

func workbookRelsXML(n int) string {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
	}
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, n+1)
	b.WriteString(`</Relationships>`)
	return b.String()
}

// stylesXML defines exactly two cell styles: 0 (normal) and 1 (bold white
// text on a brand-purple fill), used for header rows. Fill indexes 0 and 1
// ("none" and "gray125") are required placeholders Excel expects even though
// nothing uses them — index 2 is the one style actually applied.
func stylesXML() string {
	return xmlHeader + `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<fonts count="2">` +
		`<font><sz val="11"/><name val="Calibri"/></font>` +
		`<font><b/><sz val="11"/><color rgb="FFFFFFFF"/><name val="Calibri"/></font>` +
		`</fonts>` +
		`<fills count="3">` +
		`<fill><patternFill patternType="none"/></fill>` +
		`<fill><patternFill patternType="gray125"/></fill>` +
		`<fill><patternFill patternType="solid"><fgColor rgb="FF4F46E5"/><bgColor indexed="64"/></patternFill></fill>` +
		`</fills>` +
		`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
		`<cellXfs count="2">` +
		`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>` +
		`<xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/>` +
		`</cellXfs>` +
		`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>` +
		`</styleSheet>`
}

func sheetXML(rows [][]string) string {
	var b strings.Builder
	b.WriteString(xmlHeader)
	b.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)

	b.WriteString(`<sheetViews><sheetView workbookViewId="0">`)
	if len(rows) > 1 {
		b.WriteString(`<pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>`)
	}
	b.WriteString(`</sheetView></sheetViews>`)

	if widths := colWidths(rows); len(widths) > 0 {
		b.WriteString(`<cols>`)
		for i, wd := range widths {
			fmt.Fprintf(&b, `<col min="%d" max="%d" width="%.2f" customWidth="1"/>`, i+1, i+1, wd)
		}
		b.WriteString(`</cols>`)
	}

	b.WriteString(`<sheetData>`)
	for ri, row := range rows {
		fmt.Fprintf(&b, `<row r="%d">`, ri+1)
		for ci, val := range row {
			ref := cellRef(ci, ri)
			style := ""
			if ri == 0 {
				style = ` s="1"`
			} else if n, ok := parseCellNumber(val); ok {
				fmt.Fprintf(&b, `<c r="%s"%s><v>%s</v></c>`, ref, style, n)
				continue
			}
			fmt.Fprintf(&b, `<c r="%s"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, xmlSafeText(val))
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

// parseCellNumber reports whether val is safe to emit as a numeric cell,
// returning it unchanged (the CSV builders already format numbers plainly).
func parseCellNumber(val string) (string, bool) {
	if val == "" {
		return "", false
	}
	if _, err := strconv.ParseFloat(val, 64); err != nil {
		return "", false
	}
	return val, true
}

// colWidths estimates a readable column width from the longest cell in each
// column, capped to a sane range so one long question text does not blow out
// the whole sheet.
func colWidths(rows [][]string) []float64 {
	if len(rows) == 0 {
		return nil
	}
	n := len(rows[0])
	widths := make([]float64, n)
	for _, row := range rows {
		for i, v := range row {
			if i >= n {
				continue
			}
			if l := float64(len([]rune(v))); l > widths[i] {
				widths[i] = l
			}
		}
	}
	for i := range widths {
		w := widths[i] + 2
		if w < 8 {
			w = 8
		}
		if w > 42 {
			w = 42
		}
		widths[i] = w
	}
	return widths
}

func cellRef(col, row int) string { return colLetter(col) + strconv.Itoa(row+1) }

// colLetter converts a 0-indexed column number to its spreadsheet letter(s):
// 0 -> "A", 25 -> "Z", 26 -> "AA".
func colLetter(n int) string {
	n++
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	return s
}

var xmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func xmlEscape(s string) string { return xmlReplacer.Replace(s) }

// xmlSafeText strips characters XML 1.0 cannot represent (most C0 control
// codes) before escaping. Unlike the PDF export, full Unicode — emoji
// included — survives here, since OOXML text is plain UTF-8.
func xmlSafeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0x09 || r == 0x0A || r == 0x0D ||
			(r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || r >= 0x10000 {
			b.WriteRune(r)
		}
	}
	return xmlEscape(b.String())
}
