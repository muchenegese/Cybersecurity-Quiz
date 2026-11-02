# KwiqTest

A live, multiple-choice quiz platform that ships as **one executable file**.

An organiser runs the binary on a laptop; everyone else joins by scanning a QR
code or typing a short six-character code. Questions, the countdown and the
leaderboard stay in sync across every device, and when the quiz ends the
organiser downloads a complete, audit-ready report.

No installer. No database. No cloud account. No internet connection required —
a room on the same Wi-Fi is enough.

---

## Quick start

```bash
go build -o kwiqtest .    # or use a prebuilt binary
./kwiqtest
```

It prints the addresses to share and opens the organiser view:

```
  KwiqTest 1.0.0 — live quiz platform
  ──────────────────────────────────────────────────────────────

  Open the organiser view here:
      http://localhost:37390  (this computer only)
      http://192.168.1.24:37390  ← share this one with participants

  Data folder: /home/you/kwiqtest-data
  Press Ctrl+C to stop.
```

Build a quiz, press **Go live**, and project the QR code. Participants point a
phone camera at it and they are in — no app, no signup, no cap on numbers.

### Options

| Flag | Default | Purpose |
| --- | --- | --- |
| `-addr` | `:37390` | Listen address |
| `-data` | `kwiqtest-data` beside the binary | Where quizzes, sessions and uploads live |
| `-public` | request host | Base URL to put in join links and QR codes (set this behind a reverse proxy) |
| `-retain` | `72` | Hours to keep a session's data before deleting it |
| `-max-image` / `-max-video` | `5` / `50` | Upload limits, in MB |
| `-no-browser` | off | Don't open a browser on start |

Everything is portable: copy the binary and its `kwiqtest-data` folder to a USB
stick and it runs anywhere, with all quizzes and past results intact.

---

## What it does

**For the organiser**
- Build quizzes with 2–6 options per question, a per-question timer (5–120s)
  and points. Reorder, duplicate and delete questions; changes autosave.
- Attach an image or a short video to a question — or to the answer options
  themselves, for "which picture is it?" rounds. Uploads only, straight from
  the local drive, so nothing can 404 mid-quiz. Photos are downscaled and
  re-encoded automatically.
- Publish to get a join link, a six-character join code (letters and numbers,
  no ambiguous 0/O/1/I) and a downloadable QR code.
- Run the room live: start, pause, skip, repeat a question, or end early.
  Watch the answer count and the response distribution build in real time.
- Lock the room when you want to stop new arrivals — never because a limit
  was reached.
- A big-screen view for the projector, at `/#/screen/<code>`.
- Download results the moment the quiz ends.

**For participants**
- Scan or tap the link, type a name, and play. Nothing else.
- One question at a time with a synced countdown that turns amber then red.
- Answers lock on tap; instant correct/incorrect feedback with points earned.
- Lose signal mid-quiz and the phone rejoins on its own, score intact.

**Reports** (all generated server-side, so they're identical everywhere)
- **Full detailed PDF** — quiz metadata, every question exactly as shown with
  its correct answer and response distribution, then *every participant's
  answer to every question* with the option they chose, right or wrong, time
  taken and points earned, plus the attendee list and final ranking.
- **Leaderboard CSV** — rank, score, accuracy, average response time.
- **Question-wise CSV** — one row per question per participant.
- **Attendee CSV** — join time, device type, completion status.
- **Raw responses CSV** — the chronological audit trail.

---

## How it is built

```
main.go                  entry point, embeds the frontend, prints LAN URLs
internal/model           domain types
internal/store           JSON-file persistence + media storage
internal/hub             SSE fan-out, one room per session
internal/session         state machine, server-authoritative timer, scoring
internal/api             HTTP routes, uploads, QR, gzip, security headers
internal/report          CSV and PDF generation
web/                     the frontend, compiled into the binary
tests/                   Playwright browser tests (dev-only)
```

Two Go dependencies, both pure Go with no transitive dependencies of their
own: `skip2/go-qrcode` and `go-pdf/fpdf`. The frontend has **zero**
dependencies — no framework, no build step, no CDN — which is what keeps the
whole thing to one file.

### Decisions worth knowing about

**Server-sent events, not WebSockets.** Traffic is almost entirely one-way:
the server pushes state, clients POST their rare actions. SSE gets the same
sub-second sync with no upgrade handshake, no hand-rolled keepalive protocol,
no extra dependency, and browsers reconnect on their own after a dropout —
which is exactly the rejoin behaviour needed.

**Every frame is a complete snapshot, not a delta.** A backgrounded phone tab
or a lift with no signal self-heals on the next frame. There is no replay
protocol because there is nothing to replay.

**The server owns the clock.** Questions carry a server-issued deadline;
clients render a countdown against it using `performance.now()`, never the
device's wall clock. Changing a phone's time, or an NTP correction landing
mid-question, changes nothing. Late submissions are refused server-side.

**The correct answer never leaves the server early.** It is stripped from the
participant payload until the reveal, so it cannot be read out of network
traffic or the DOM.

**Files instead of a database.** Live state is in memory — that is what makes
the realtime path fast — and mirrored to plain JSON, written atomically. It
means no CGO, no install step, and a data folder you can read with `cat`.

**No participant cap.** Joining appends to a slice. The only guard is a
per-IP join rate limit, which blunts a bot flooding a public link without ever
turning away a real room full of people.

### Deliberate limitations

- The PDF uses the standard PDF core fonts, which cover Latin-1. Emoji are
  dropped and other scripts render as `?`. Shipping a full Unicode font would
  add close to a megabyte to a binary whose point is being small. **The CSV
  exports are full-fidelity UTF-8**, so nothing is actually lost.
- Videos cannot be embedded in a PDF, so the report records the filename and
  the fact that the question had a video prompt.
- WebP and animated GIF uploads are stored as-is rather than re-encoded
  (Go's standard library cannot decode WebP, and re-encoding a GIF would drop
  its animation). They are still size-checked.
- Single process. It scales comfortably to the size of room a laptop and a
  Wi-Fi access point can serve; it is not a multi-node cloud deployment.

---

## Testing

```bash
go test ./...            # unit and HTTP-level tests
go test -race ./...      # same, with the race detector

cd tests
npm install
npx playwright install chromium webkit
npx playwright test                     # everything, all device profiles
npx playwright test --project=iphone    # one profile
```

**Go tests** cover scoring (both modes), the session state machine, pause/
resume time preservation, question repeat rollback, rejoin-with-score,
duplicate-name handling, 300 concurrent participants, answer validation and
finality, information leakage between roles, persistence across a restart,
retention purging, upload validation and compression, gzip behaviour, and
report completeness — including an assertion that the PDF contains every
question, option and participant.

**Browser tests** run against the real binary across five device profiles —
iPhone 13, iPhone SE, Pixel 7, a 320px budget phone and an iPad — plus desktop.
They cover authoring, a full mixed-device session (organiser on a laptop with
participants on an iPhone, a Pixel and an iPad), organiser pacing controls,
room locking, reconnect-with-score, the projector view, a 12-participant room,
QR entry, clock-tampering resistance, and the layout failures that only appear
on a real phone: horizontal overflow, touch targets under 44px, inputs that
trigger iOS zoom-on-focus, options that need scrolling to reach, and rotation
mid-question. An accessibility suite checks WCAG AA contrast on every screen,
keyboard-only play, focus visibility, and colour-independent answer coding.

---

## Accessibility & performance notes

- Dark, high-contrast theme; every text/background pair is checked against
  WCAG 2.1 AA in the test suite.
- Answer options carry a distinct shape (▲ ◆ ● ■ ★ ✦) as well as a colour, so
  they stay distinguishable with any form of colour vision.
- All interactive elements are at least 44px tall; inputs are 16px so iOS
  doesn't zoom on focus; safe-area insets keep content clear of the notch.
- `prefers-reduced-motion` disables every animation.
- First load is ~31 KB gzipped over five requests. The binary is ~7.4 MB.

---

## Security

- Session codes are randomly generated and time-bound; session data is deleted
  after the retention window.
- No PII beyond a display name. No accounts, no email, no tracking.
- Organiser identity is a 128-bit bearer token held by the browser — that is
  what makes hosting without a login work while keeping quizzes and reports
  private to whoever created them.
- Strict Content-Security-Policy; the app loads nothing from any other origin.
- Uploads are identified by magic bytes, not by their filename.

Run it behind a TLS-terminating reverse proxy if participants are joining from
outside your own network, and set `-public` to the external URL so join links
and QR codes point to the right place.
