package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/genai"
)

const (
	// historyFileSuffix names a session's archive next to its transcript:
	// <id>.json and <id>.history.db.
	historyFileSuffix = ".history.db"

	historySchemaVersion = 1

	// historyLineCap and historyReadCap bound one archive line and one read:
	// a minified payload can be a single enormous line.
	historyLineCap = 2000
	historyReadCap = 30_000

	// historySnippetCap is how much of a matching line a search hit shows.
	historySnippetCap = 240
)

var errHistoryEmpty = errors.New("nothing has been archived in this conversation yet: auto-compaction has not cut any history")

const (
	recallBlockOpen  = "<memory>\n"
	recallBlockClose = "\n</memory>"
)

// RecallBlock wraps a recalled-memory block the way the recall hook appends
// it to the turn's user message. It is a per-request view, not conversation,
// so the history archive leaves it out.
func RecallBlock(block string) string {
	return recallBlockOpen + block + recallBlockClose
}

func isRecallBlock(text string) bool {
	return strings.HasPrefix(text, recallBlockOpen) && strings.HasSuffix(text, recallBlockClose)
}

// HistoryArchive keeps what auto-compaction cut out of one agent's view.
// Every cut's dropped contents become one numbered chunk, rendered to text
// whose line numbers run on across chunks, indexed for keyword search, and
// summarized. It is SQLite with an FTS4 index (FTS5 needs a build tag every
// importer of this package would then need). A file-backed archive lives next
// to the session transcript and is created with its first chunk; an
// in-memory one lives until Close.
type HistoryArchive struct {
	path string

	mu     sync.Mutex
	db     *sql.DB
	loaded bool
	chunks []HistoryChunk
}

// HistoryChunk is one archived cut.
type HistoryChunk struct {
	ID         int
	CreatedAt  time.Time
	FirstEntry int
	LastEntry  int
	FirstLine  int
	LastLine   int
	// Tokens is the compactor's estimate of the dropped contents.
	Tokens int

	Headline string
	Summary  string
	// SummaryError is the last failed summary attempt's error, and
	// SummaryAttempts how many attempts failed.
	SummaryError    string
	SummaryAttempts int

	// start marks the first archived content, end the first content after
	// the chunk: the watermark that cut left standing.
	start contentMark
	end   contentMark
}

// Summarized reports whether the chunk has a summary.
func (c HistoryChunk) Summarized() bool { return c.Summary != "" }

// NewHistoryArchive returns the archive stored at path. Nothing is created
// on disk until the first chunk is archived.
func NewHistoryArchive(path string) *HistoryArchive {
	return &HistoryArchive{path: path}
}

// NewMemoryHistoryArchive returns an archive that lives until Close, for an
// agent whose session is not persisted (a workflow step).
func NewMemoryHistoryArchive() *HistoryArchive {
	return &HistoryArchive{}
}

// HistoryArchivePath is where a session's archive lives: next to its
// transcript when that exists, else where the transcript will be created.
func HistoryArchivePath(svc *FileSessionService, cwd, sessionID string) (string, error) {
	if p, err := svc.PathFor(sessionID); err == nil {
		return strings.TrimSuffix(p, ".json") + historyFileSuffix, nil
	}
	dir, err := svc.DirFor(cwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionID+historyFileSuffix), nil
}

// Path is the archive's database file, empty for an in-memory archive.
func (a *HistoryArchive) Path() string { return a.path }

// Close releases the database. The archive reopens on next use.
func (a *HistoryArchive) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.db == nil {
		return nil
	}
	err := a.db.Close()
	a.db = nil
	a.loaded = false
	a.chunks = nil
	return err
}

// Chunks returns every archived chunk, oldest first.
func (a *HistoryArchive) Chunks() ([]HistoryChunk, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil {
		return nil, err
	}
	return append([]HistoryChunk(nil), a.chunks...), nil
}

// Chunk returns one chunk by id.
func (a *HistoryArchive) Chunk(id int) (HistoryChunk, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil {
		return HistoryChunk{}, err
	}
	return a.chunkLocked(id)
}

func (a *HistoryArchive) chunkLocked(id int) (HistoryChunk, error) {
	if len(a.chunks) == 0 {
		return HistoryChunk{}, errHistoryEmpty
	}
	if id < 1 || id > len(a.chunks) {
		return HistoryChunk{}, fmt.Errorf("no archived chunk #%d: the archive holds chunks #1–#%d", id, len(a.chunks))
	}
	return a.chunks[id-1], nil
}

// openLocked opens the database. A file archive that does not exist yet is
// only created when create is set; until then it reads as empty.
func (a *HistoryArchive) openLocked(create bool) error {
	if a.db != nil {
		return nil
	}
	if a.loaded && !create {
		return nil
	}
	var dsn string
	if a.path == "" {
		if !create {
			a.loaded = true
			return nil
		}
		dsn = ":memory:"
	} else {
		if _, err := os.Stat(a.path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if !create {
				a.loaded = true
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
				return err
			}
		}
		dsn = a.path + "?_busy_timeout=5000"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	// One connection: an in-memory database exists per connection, and a
	// file one is only ever written by this archive.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := migrateHistory(db); err != nil {
		db.Close()
		return err
	}
	chunks, err := loadHistoryChunks(db)
	if err != nil {
		db.Close()
		return err
	}
	if a.path != "" {
		_ = os.Chmod(a.path, 0o600)
	}
	a.db = db
	a.loaded = true
	a.chunks = chunks
	return nil
}

func migrateHistory(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	switch {
	case version == historySchemaVersion:
		return nil
	case version > historySchemaVersion:
		return fmt.Errorf("history archive schema %d is newer than this ask (%d)", version, historySchemaVersion)
	}
	stmts := []string{
		`CREATE TABLE chunks (
			id INTEGER PRIMARY KEY,
			created_at INTEGER NOT NULL,
			first_entry INTEGER NOT NULL,
			last_entry INTEGER NOT NULL,
			first_line INTEGER NOT NULL,
			last_line INTEGER NOT NULL,
			tokens INTEGER NOT NULL,
			start_fingerprint TEXT NOT NULL,
			start_ordinal INTEGER NOT NULL,
			end_fingerprint TEXT NOT NULL,
			end_ordinal INTEGER NOT NULL,
			headline TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			summary_error TEXT NOT NULL DEFAULT '',
			summary_attempts INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE entries (
			id INTEGER PRIMARY KEY,
			chunk INTEGER NOT NULL,
			first_line INTEGER NOT NULL,
			line_count INTEGER NOT NULL,
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			body TEXT NOT NULL
		)`,
		`CREATE INDEX entries_by_line ON entries(first_line)`,
		`CREATE VIRTUAL TABLE entries_fts USING fts4(content="entries", name, body, tokenize=unicode61)`,
		fmt.Sprintf(`PRAGMA user_version = %d`, historySchemaVersion),
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			tx.Rollback()
			return fmt.Errorf("history archive schema: %w", err)
		}
	}
	return tx.Commit()
}

func loadHistoryChunks(db *sql.DB) ([]HistoryChunk, error) {
	rows, err := db.Query(`SELECT id, created_at, first_entry, last_entry, first_line, last_line, tokens,
		start_fingerprint, start_ordinal, end_fingerprint, end_ordinal,
		headline, summary, summary_error, summary_attempts
		FROM chunks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryChunk
	for rows.Next() {
		var c HistoryChunk
		var created int64
		if err := rows.Scan(&c.ID, &created, &c.FirstEntry, &c.LastEntry, &c.FirstLine, &c.LastLine, &c.Tokens,
			&c.start.fingerprint, &c.start.ordinal, &c.end.fingerprint, &c.end.ordinal,
			&c.Headline, &c.Summary, &c.SummaryError, &c.SummaryAttempts); err != nil {
			return nil, err
		}
		c.CreatedAt = time.UnixMilli(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// historyEntry is one archived message part, as the archive renders it: a
// header line naming what it was, then its body.
type historyEntry struct {
	kind string
	name string
	body string
}

const (
	historyKindUser       = "user"
	historyKindAssistant  = "assistant"
	historyKindThinking   = "thinking"
	historyKindToolCall   = "tool call"
	historyKindToolResult = "tool result"
	historyKindToolError  = "tool error"
	historyKindImage      = "image"
	historyKindFile       = "file"
)

func (e historyEntry) header(id int) string {
	if e.name != "" {
		return fmt.Sprintf("[entry %d: %s %s]", id, e.kind, e.name)
	}
	return fmt.Sprintf("[entry %d: %s]", id, e.kind)
}

func (e historyEntry) bodyLines() []string {
	return strings.Split(e.body, "\n")
}

// historyEntries renders contents as archive entries in wire order. Text
// runs of one kind merge; every tool call, tool result, and attachment is an
// entry of its own. Thought signatures are dropped, and so is the recalled
// memory block a request hook appends to the turn's user message.
func historyEntries(contents []*genai.Content) []historyEntry {
	var out []historyEntry
	for _, c := range contents {
		out = append(out, historyContentEntries(c)...)
	}
	return out
}

func historyContentEntries(c *genai.Content) []historyEntry {
	if c == nil {
		return nil
	}
	var out []historyEntry
	var text strings.Builder
	textKind := ""
	flush := func() {
		if text.Len() > 0 {
			out = append(out, newHistoryEntry(textKind, "", text.String()))
		}
		text.Reset()
		textKind = ""
	}
	for _, p := range c.Parts {
		switch {
		case p == nil:
		case p.FunctionCall != nil:
			flush()
			args := "{}"
			if len(p.FunctionCall.Args) > 0 {
				if b, err := json.MarshalIndent(p.FunctionCall.Args, "", "  "); err == nil {
					args = string(b)
				}
			}
			out = append(out, newHistoryEntry(historyKindToolCall, p.FunctionCall.Name, args))
		case p.FunctionResponse != nil:
			flush()
			kind := historyKindToolResult
			resp, isErr := ToolResultText(p.FunctionResponse.Response)
			if isErr {
				kind = historyKindToolError
			}
			out = append(out, newHistoryEntry(kind, p.FunctionResponse.Name, resp))
		case p.InlineData != nil:
			flush()
			out = append(out, newHistoryEntry(attachmentKind(p.InlineData.MIMEType), "",
				fmt.Sprintf("[%s, %s]", p.InlineData.MIMEType, formatByteSize(len(p.InlineData.Data)))))
		case p.FileData != nil:
			flush()
			out = append(out, newHistoryEntry(attachmentKind(p.FileData.MIMEType), "",
				fmt.Sprintf("[%s, %s]", p.FileData.MIMEType, p.FileData.FileURI)))
		case p.Text != "":
			kind := historyKindUser
			switch {
			case p.Thought:
				kind = historyKindThinking
			case c.Role == genai.RoleModel:
				kind = historyKindAssistant
			case isRecallBlock(p.Text):
				continue
			}
			if kind != textKind {
				flush()
				textKind = kind
			}
			text.WriteString(p.Text)
		}
	}
	flush()
	return out
}

func newHistoryEntry(kind, name, body string) historyEntry {
	body = strings.TrimRight(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	if strings.TrimSpace(body) == "" {
		body = "(empty)"
	}
	return historyEntry{kind: kind, name: name, body: body}
}

func attachmentKind(mime string) string {
	if strings.HasPrefix(mime, "image/") {
		return historyKindImage
	}
	return historyKindFile
}

func formatByteSize(n int) string {
	switch {
	case n < 1024:
		return strconv.Itoa(n) + " bytes"
	case n < 1024*1024:
		return strconv.Itoa((n+512)/1024) + " KB"
	default:
		return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + " MB"
	}
}

// appendChunk archives contents as the next chunk. start marks contents[0];
// end marks the first content after them, the cut's watermark.
func (a *HistoryArchive) appendChunk(contents []*genai.Content, start, end contentMark, tokens int) (HistoryChunk, error) {
	entries := historyEntries(contents)
	if len(entries) == 0 {
		return HistoryChunk{}, errors.New("nothing to archive")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(true); err != nil {
		return HistoryChunk{}, err
	}
	c := HistoryChunk{
		ID:         len(a.chunks) + 1,
		CreatedAt:  time.Now(),
		FirstEntry: 1,
		FirstLine:  1,
		Tokens:     tokens,
		start:      start,
		end:        end,
	}
	if n := len(a.chunks); n > 0 {
		c.FirstEntry = a.chunks[n-1].LastEntry + 1
		c.FirstLine = a.chunks[n-1].LastLine + 1
	}

	tx, err := a.db.Begin()
	if err != nil {
		return HistoryChunk{}, err
	}
	insEntry, err := tx.Prepare(`INSERT INTO entries (id, chunk, first_line, line_count, kind, name, body) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return HistoryChunk{}, err
	}
	defer insEntry.Close()
	insFTS, err := tx.Prepare(`INSERT INTO entries_fts (docid, name, body) VALUES (?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return HistoryChunk{}, err
	}
	defer insFTS.Close()

	id, line := c.FirstEntry, c.FirstLine
	for _, e := range entries {
		count := 1 + len(e.bodyLines())
		if _, err := insEntry.Exec(id, c.ID, line, count, e.kind, e.name, e.body); err != nil {
			tx.Rollback()
			return HistoryChunk{}, err
		}
		if _, err := insFTS.Exec(id, e.name, e.body); err != nil {
			tx.Rollback()
			return HistoryChunk{}, err
		}
		id++
		line += count
	}
	c.LastEntry, c.LastLine = id-1, line-1

	if _, err := tx.Exec(`INSERT INTO chunks (id, created_at, first_entry, last_entry, first_line, last_line, tokens,
		start_fingerprint, start_ordinal, end_fingerprint, end_ordinal) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.CreatedAt.UnixMilli(), c.FirstEntry, c.LastEntry, c.FirstLine, c.LastLine, c.Tokens,
		c.start.fingerprint, c.start.ordinal, c.end.fingerprint, c.end.ordinal); err != nil {
		tx.Rollback()
		return HistoryChunk{}, err
	}
	if err := tx.Commit(); err != nil {
		return HistoryChunk{}, err
	}
	a.chunks = append(a.chunks, c)
	return c, nil
}

// setSummary stores a chunk's summary.
func (a *HistoryArchive) setSummary(id int, headline, summary string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil {
		return err
	}
	if _, err := a.chunkLocked(id); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE chunks SET headline = ?, summary = ?, summary_error = '' WHERE id = ?`, headline, summary, id); err != nil {
		return err
	}
	c := &a.chunks[id-1]
	c.Headline, c.Summary, c.SummaryError = headline, summary, ""
	return nil
}

// recordSummaryFailure counts one failed summary attempt.
func (a *HistoryArchive) recordSummaryFailure(id int, msg string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil {
		return err
	}
	if _, err := a.chunkLocked(id); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE chunks SET summary_error = ?, summary_attempts = summary_attempts + 1 WHERE id = ?`, msg, id); err != nil {
		return err
	}
	c := &a.chunks[id-1]
	c.SummaryError = msg
	c.SummaryAttempts++
	return nil
}

// count is how many chunks the archive holds; a nil archive holds none.
func (a *HistoryArchive) count() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil {
		return 0
	}
	return len(a.chunks)
}

// lastEnd is the watermark the newest chunk's cut left: everything before
// it is archived.
func (a *HistoryArchive) lastEnd() (contentMark, bool) {
	if a == nil {
		return contentMark{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil || len(a.chunks) == 0 {
		return contentMark{}, false
	}
	return a.chunks[len(a.chunks)-1].end, true
}

// chunkText renders one chunk in full, without line numbers.
func (a *HistoryArchive) chunkText(id int) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil {
		return "", err
	}
	c, err := a.chunkLocked(id)
	if err != nil {
		return "", err
	}
	rows, err := a.db.Query(`SELECT id, kind, name, body FROM entries WHERE chunk = ? ORDER BY id`, c.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id int
		var e historyEntry
		if err := rows.Scan(&id, &e.kind, &e.name, &e.body); err != nil {
			return "", err
		}
		b.WriteString(e.header(id))
		b.WriteByte('\n')
		b.WriteString(e.body)
		b.WriteByte('\n')
	}
	return b.String(), rows.Err()
}

// historyLine is one numbered archive line.
type historyLine struct {
	n     int
	chunk int
	text  string
}

// historyRead is one window of archive lines.
type historyRead struct {
	lines []historyLine
	// next is where to continue in the same direction, 0 at the archive's
	// edge.
	next  int
	total int
}

// read returns up to limit lines starting at line, or at chunk's edge, or
// at the newest chunk's edge: its first line reading forward, its last
// reading backward. Lines come back in ascending order either way.
func (a *HistoryArchive) read(line, chunk int, backward bool, limit int) (historyRead, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil {
		return historyRead{}, err
	}
	if len(a.chunks) == 0 {
		return historyRead{}, errHistoryEmpty
	}
	total := a.chunks[len(a.chunks)-1].LastLine
	anchor := line
	switch {
	case line != 0:
		if line < 1 || line > total {
			return historyRead{}, fmt.Errorf("line %d is outside the archive, which holds lines 1–%d", line, total)
		}
	default:
		id := chunk
		if id == 0 {
			id = len(a.chunks)
		}
		c, err := a.chunkLocked(id)
		if err != nil {
			return historyRead{}, err
		}
		anchor = c.FirstLine
		if backward {
			anchor = c.LastLine
		}
	}
	lo, hi := anchor, min(total, anchor+limit-1)
	if backward {
		lo, hi = max(1, anchor-limit+1), anchor
	}
	lines, err := a.linesLocked(lo, hi)
	if err != nil {
		return historyRead{}, err
	}

	out := historyRead{total: total}
	if len(lines) == 0 {
		return out, nil
	}
	size := 0
	if backward {
		i := len(lines) - 1
		for ; i >= 0; i-- {
			size += len(lines[i].text) + 8
			if size > historyReadCap && i < len(lines)-1 {
				break
			}
		}
		lines = lines[i+1:]
		if first := lines[0].n; first > 1 {
			out.next = first - 1
		}
	} else {
		i := 0
		for ; i < len(lines); i++ {
			size += len(lines[i].text) + 8
			if size > historyReadCap && i > 0 {
				break
			}
		}
		lines = lines[:i]
		if last := lines[len(lines)-1].n; last < total {
			out.next = last + 1
		}
	}
	out.lines = lines
	return out, nil
}

// linesLocked returns archive lines lo..hi, each clipped to historyLineCap.
func (a *HistoryArchive) linesLocked(lo, hi int) ([]historyLine, error) {
	rows, err := a.db.Query(`SELECT id, chunk, first_line, kind, name, body FROM entries
		WHERE first_line >= (SELECT MAX(first_line) FROM entries WHERE first_line <= ?) AND first_line <= ?
		ORDER BY first_line`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []historyLine
	for rows.Next() {
		var id, chunk, first int
		var e historyEntry
		if err := rows.Scan(&id, &chunk, &first, &e.kind, &e.name, &e.body); err != nil {
			return nil, err
		}
		all := append([]string{e.header(id)}, e.bodyLines()...)
		for i, text := range all {
			n := first + i
			if n < lo || n > hi {
				continue
			}
			out = append(out, historyLine{n: n, chunk: chunk, text: clipHistoryLine(text)})
		}
	}
	return out, rows.Err()
}

func clipHistoryLine(s string) string {
	if len(s) <= historyLineCap {
		return s
	}
	cut := historyLineCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf(" … [line cut, %d more bytes]", len(s)-cut)
}

// historyHit is one matching archive line.
type historyHit struct {
	chunk   int
	line    int
	kind    string
	name    string
	snippet string
}

// search finds archive lines matching query, newest first, at most limit.
// A keyword query is FTS4 syntax (words that must all appear, "phrases",
// prefix*, OR, NOT); literal matches query as a case-insensitive substring.
// chunk, when non-zero, confines the search to one chunk. more reports
// whether matches were left out.
func (a *HistoryArchive) search(query string, literal bool, chunk, limit int) (hits []historyHit, more bool, err error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, errors.New("query is required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.openLocked(false); err != nil {
		return nil, false, err
	}
	if len(a.chunks) == 0 {
		return nil, false, errHistoryEmpty
	}
	if chunk != 0 {
		if _, err := a.chunkLocked(chunk); err != nil {
			return nil, false, err
		}
	}
	if literal {
		return a.searchLiteralLocked(query, chunk, limit)
	}
	hits, more, err = a.searchFTSLocked(query, chunk, limit)
	if err != nil && isFTSSyntaxError(err) {
		quoted := quoteFTSTerms(query)
		if quoted != "" && quoted != query {
			hits, more, err = a.searchFTSLocked(quoted, chunk, limit)
		}
		if err != nil {
			return nil, false, fmt.Errorf("%q is not a valid keyword query (%v); retry with literal set to match it as exact text", query, err)
		}
	}
	return hits, more, err
}

func isFTSSyntaxError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "malformed match") || strings.Contains(msg, "fts") ||
		strings.Contains(msg, "syntax error") || strings.Contains(msg, "unterminated")
}

// quoteFTSTerms turns every whitespace-separated term into a phrase, which
// is always valid syntax and keeps the all-terms meaning.
func quoteFTSTerms(query string) string {
	var terms []string
	for _, f := range strings.Fields(query) {
		f = strings.ReplaceAll(f, `"`, "")
		if f != "" {
			terms = append(terms, `"`+f+`"`)
		}
	}
	return strings.Join(terms, " ")
}

type ftsMatch struct {
	docid   int
	offsets string
}

func (a *HistoryArchive) searchFTSLocked(query string, chunk, limit int) ([]historyHit, bool, error) {
	q := `SELECT docid, offsets(entries_fts) FROM entries_fts WHERE entries_fts MATCH ? ORDER BY docid DESC`
	args := []any{query}
	if chunk != 0 {
		c := a.chunks[chunk-1]
		q = `SELECT docid, offsets(entries_fts) FROM entries_fts WHERE entries_fts MATCH ? AND docid BETWEEN ? AND ? ORDER BY docid DESC`
		args = append(args, c.FirstEntry, c.LastEntry)
	}
	rows, err := a.db.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	var matches []ftsMatch
	for rows.Next() {
		var m ftsMatch
		if err := rows.Scan(&m.docid, &m.offsets); err != nil {
			rows.Close()
			return nil, false, err
		}
		matches = append(matches, m)
		// Every entry yields at least one hit; one past the limit is
		// enough to know there are more.
		if len(matches) > limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	rows.Close()

	var hits []historyHit
	for _, m := range matches {
		e, chunkID, first, err := a.entryLocked(m.docid)
		if err != nil {
			return nil, false, err
		}
		nums := strings.Fields(m.offsets)
		var spots []matchSpot
		for i := 0; i+3 < len(nums); i += 4 {
			col, _ := strconv.Atoi(nums[i])
			off, _ := strconv.Atoi(nums[i+2])
			spots = append(spots, matchSpot{column: col, offset: off})
		}
		hits = append(hits, entryHits(m.docid, e, chunkID, first, spots)...)
		if len(hits) > limit {
			return hits[:limit], true, nil
		}
	}
	return hits, len(matches) > limit, nil
}

func (a *HistoryArchive) searchLiteralLocked(query string, chunk, limit int) ([]historyHit, bool, error) {
	needle := asciiLower(query)
	q := `SELECT id, chunk, first_line, kind, name, body FROM entries
		WHERE (instr(lower(body), ?) > 0 OR instr(lower(name), ?) > 0) ORDER BY id DESC`
	args := []any{needle, needle}
	if chunk != 0 {
		q = `SELECT id, chunk, first_line, kind, name, body FROM entries
			WHERE chunk = ? AND (instr(lower(body), ?) > 0 OR instr(lower(name), ?) > 0) ORDER BY id DESC`
		args = []any{chunk, needle, needle}
	}
	rows, err := a.db.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var hits []historyHit
	for rows.Next() {
		var id, chunkID, first int
		var e historyEntry
		if err := rows.Scan(&id, &chunkID, &first, &e.kind, &e.name, &e.body); err != nil {
			return nil, false, err
		}
		var spots []matchSpot
		if strings.Contains(asciiLower(e.name), needle) {
			spots = append(spots, matchSpot{column: 0})
		}
		lowered := asciiLower(e.body)
		for from := 0; ; {
			i := strings.Index(lowered[from:], needle)
			if i < 0 {
				break
			}
			spots = append(spots, matchSpot{column: 1, offset: from + i})
			from += i + len(needle)
		}
		hits = append(hits, entryHits(id, e, chunkID, first, spots)...)
		if len(hits) > limit {
			return hits[:limit], true, nil
		}
	}
	return hits, false, rows.Err()
}

func (a *HistoryArchive) entryLocked(id int) (historyEntry, int, int, error) {
	var e historyEntry
	var chunk, first int
	err := a.db.QueryRow(`SELECT chunk, first_line, kind, name, body FROM entries WHERE id = ?`, id).
		Scan(&chunk, &first, &e.kind, &e.name, &e.body)
	return e, chunk, first, err
}

// matchSpot is one match inside an entry: column 0 is the tool name (its
// header line), column 1 the body at a byte offset.
type matchSpot struct {
	column int
	offset int
}

// entryHits maps entry id's matches to its lines, one hit per line.
func entryHits(id int, e historyEntry, chunk, first int, spots []matchSpot) []historyHit {
	seen := map[int]bool{}
	var out []historyHit
	for _, s := range spots {
		n, text, at := first, "", 0
		if s.column == 0 {
			text = e.header(id)
		} else {
			if s.offset < 0 || s.offset > len(e.body) {
				continue
			}
			start := strings.LastIndexByte(e.body[:s.offset], '\n') + 1
			end := strings.IndexByte(e.body[s.offset:], '\n')
			if end < 0 {
				end = len(e.body)
			} else {
				end += s.offset
			}
			n = first + 1 + strings.Count(e.body[:start], "\n")
			text, at = e.body[start:end], s.offset-start
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, historyHit{chunk: chunk, line: n, kind: e.kind, name: e.name, snippet: snippetAround(text, at)})
	}
	return out
}

// snippetAround clips a line to historySnippetCap bytes around byte at.
func snippetAround(line string, at int) string {
	line = strings.TrimSpace(line)
	if len(line) <= historySnippetCap {
		return line
	}
	start := max(0, at-historySnippetCap/3)
	end := min(len(line), start+historySnippetCap)
	start = max(0, end-historySnippetCap)
	for start > 0 && !utf8.RuneStart(line[start]) {
		start--
	}
	for end < len(line) && !utf8.RuneStart(line[end]) {
		end++
	}
	out := line[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(line) {
		out += "…"
	}
	return out
}

// asciiLower lowercases ASCII letters only, which keeps byte offsets
// aligned with the original and matches SQLite's lower().
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
