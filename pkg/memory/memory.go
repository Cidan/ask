package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

var _ adkmemory.Service = (*Service)(nil)

var (
	defaultMu  sync.RWMutex
	defaultSvc *Service
)

// Concept kinds and the global scope.
const (
	KindUser      = "user"
	KindFeedback  = "feedback"
	KindProject   = "project"
	KindReference = "reference"

	ScopeGlobal = "global"
)

// Kinds lists every valid concept kind.
var Kinds = []string{KindUser, KindFeedback, KindProject, KindReference}

// ValidKind reports whether k is one of Kinds.
func ValidKind(k string) bool {
	for _, kind := range Kinds {
		if kind == k {
			return true
		}
	}
	return false
}

// ScopeFor maps a working directory to its memory scope: the project
// root, falling back to the cleaned cwd. An empty cwd has no scope.
func ScopeFor(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	if root := config.ProjectRoot(cwd); root != "" {
		return root
	}
	return filepath.Clean(cwd)
}

// Concept is one long-term memory: a one-line title that goes into
// prompts cheaply, a body fetched on demand, and a decaying weight.
type Concept struct {
	ID          int64     `json:"id"`
	Scope       string    `json:"scope"`
	Kind        string    `json:"kind"`
	Topic       string    `json:"topic,omitempty"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Weight      float64   `json:"weight"`
	AccessCount int64     `json:"access_count"`
	CreatedAt   time.Time `json:"created_at"`
	LastTouched time.Time `json:"last_touched"`

	// Distance and Score are set on recall hits only.
	Distance float32 `json:"distance,omitempty"`
	Score    float64 `json:"score,omitempty"`
}

// Topic is a short label concepts are grouped under, weighted like a
// concept so stale topics fall out of the candidate list.
type Topic struct {
	ID          int64     `json:"id"`
	Scope       string    `json:"scope"`
	Name        string    `json:"name"`
	Weight      float64   `json:"weight"`
	LastTouched time.Time `json:"last_touched"`
}

// RecallQuery describes one recall.
type RecallQuery struct {
	// Cwd selects the project scope; global concepts are always included.
	// Empty searches every scope.
	Cwd string
	// Query is embedded and matched against every concept.
	Query string
	// Topic is the caller's current topic, used when the hits do not agree
	// on one and to boost same-topic candidates.
	Topic string
	K     int
	// Silent skips the implicit weight bump on the returned concepts.
	Silent bool
}

// RecallResult is the ranked hits plus the topic inferred from them.
type RecallResult struct {
	Concepts []Concept
	// Topic is the dominant topic among the candidates, else Query.Topic.
	Topic string
}

// TurnRecord is one finished conversational turn handed to an Extractor.
type TurnRecord struct {
	Cwd      string
	Prompt   string
	Response string
	Topic    string
	Files    []string
}

// Extractor turns finished turns into concepts, asynchronously.
type Extractor interface {
	Enqueue(TurnRecord) bool
}

// Options configures the initialization of the Memory Service.
type Options struct {
	DBPath string
	// Embedder produces the vectors the store indexes; required.
	Embedder Embedder
	// Now is the clock decay and refractory math read; nil means time.Now.
	Now func() time.Time
}

// Service manages concept persistence, retrieval, and weighting.
type Service struct {
	db        *sql.DB
	embedder  Embedder
	now       func() time.Time
	extractor Extractor
	mu        sync.RWMutex
}

// DefaultDBPath returns the standard SQLite vector database file location.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "memory", "memory.db"), nil
}

// NewService initializes a SQLite-vec memory service with the specified options.
func NewService(opts Options) (*Service, error) {
	if opts.Embedder == nil {
		return nil, errors.New("memory: Options.Embedder is required")
	}
	embedder := opts.Embedder

	dbPath := opts.DBPath
	if dbPath == "" {
		var err error
		dbPath, err = DefaultDBPath()
		if err != nil {
			embedder.Close()
			return nil, err
		}
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
		embedder.Close()
		return nil, err
	}

	sqlite_vec.Auto()
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
	if err != nil {
		embedder.Close()
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	svc := &Service{db: db, embedder: embedder, now: now}
	if err := svc.initSchema(); err != nil {
		db.Close()
		embedder.Close()
		return nil, err
	}
	return svc, nil
}

func (s *Service) initSchema() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS memory_concepts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL,
			kind TEXT NOT NULL,
			topic TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			weight REAL NOT NULL,
			access_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			last_touched INTEGER NOT NULL,
			last_explicit INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS memory_concepts_scope ON memory_concepts(scope);
		CREATE TABLE IF NOT EXISTS memory_topics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL,
			name TEXT NOT NULL,
			weight REAL NOT NULL,
			created_at INTEGER NOT NULL,
			last_touched INTEGER NOT NULL,
			UNIQUE(scope, name)
		);
	`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`
		CREATE VIRTUAL TABLE IF NOT EXISTS vec_concepts USING vec0(
			embedding float[%d]
		);
	`, s.embedder.EmbdSize()))
	if err != nil {
		return err
	}
	return s.migrateLegacy()
}

// migrateLegacy converts the pre-concept store (one raw text row per
// memory, no weights, no scope beyond project id) into concepts, then
// drops the old tables. Runs once; a store without them is a no-op.
func (s *Service) migrateLegacy() error {
	var name string
	err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='project_memory'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT project_id, text_payload, last_recalled_at FROM project_memory ORDER BY id`)
	if err != nil {
		return err
	}
	type legacyRow struct {
		project string
		text    string
		touched time.Time
	}
	var legacy []legacyRow
	for rows.Next() {
		var r legacyRow
		var touched sql.NullTime
		if err := rows.Scan(&r.project, &r.text, &touched); err != nil {
			rows.Close()
			return err
		}
		if touched.Valid {
			r.touched = touched.Time
		}
		legacy = append(legacy, r)
	}
	rows.Close()

	ctx := context.Background()
	for _, r := range legacy {
		text := strings.TrimSpace(r.text)
		if text == "" {
			continue
		}
		scope := strings.TrimSpace(r.project)
		if scope == "" {
			scope = ScopeGlobal
		}
		touched := r.touched
		if touched.IsZero() {
			touched = s.now()
		}
		c := Concept{
			Scope:       scope,
			Kind:        KindProject,
			Title:       TitleFromBody(text),
			Body:        text,
			CreatedAt:   touched,
			LastTouched: touched,
		}
		if _, err := s.Upsert(ctx, c); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`DROP TABLE IF EXISTS vec_memory; DROP TABLE IF EXISTS project_memory;`)
	return err
}

// TitleFromBody derives a one-line title: the first non-empty line,
// clipped to 80 runes.
func TitleFromBody(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) > 80 {
			return strings.TrimSpace(string(r[:79])) + "…"
		}
		return line
	}
	return ""
}

// IsOpen reports whether the memory service is initialized and open.
func (s *Service) IsOpen() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.db != nil && s.embedder != nil
}

// Close releases the database and embedding model.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	if s.embedder != nil {
		s.embedder.Close()
		s.embedder = nil
	}
	if s.db != nil {
		err = s.db.Close()
		s.db = nil
	}
	return err
}

// SetExtractor installs the Extractor AddSessionToMemory hands turns to.
func (s *Service) SetExtractor(e Extractor) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extractor = e
}

func (s *Service) handles() (*sql.DB, Embedder, error) {
	if s == nil {
		return nil, nil, errors.New("memory service closed")
	}
	s.mu.RLock()
	db, model := s.db, s.embedder
	s.mu.RUnlock()
	if db == nil || model == nil {
		return nil, nil, errors.New("memory service closed")
	}
	return db, model, nil
}

// SerializeVector converts a float32 slice to the byte representation expected by sqlite-vec.
func SerializeVector(vec []float32) []byte {
	b, _ := sqlite_vec.SerializeFloat32(vec)
	return b
}

func embeddingText(c Concept) string {
	if strings.TrimSpace(c.Body) == "" {
		return c.Title
	}
	return c.Title + "\n" + c.Body
}

// Upsert stores c: a zero ID inserts a new concept at WeightInitial; a
// positive ID rewrites that concept's text and re-embeds it, treating the
// rewrite as an implicit bump. Returns the concept id.
func (s *Service) Upsert(ctx context.Context, c Concept) (int64, error) {
	db, model, err := s.handles()
	if err != nil {
		return 0, err
	}
	c.Title = strings.TrimSpace(c.Title)
	c.Body = strings.TrimSpace(c.Body)
	if c.Title == "" {
		c.Title = TitleFromBody(c.Body)
	}
	if c.Title == "" {
		return 0, errors.New("concept needs a title or a body")
	}
	if c.Body == "" {
		c.Body = c.Title
	}
	c.Scope = strings.TrimSpace(c.Scope)
	if c.Scope == "" {
		return 0, errors.New("concept needs a scope")
	}
	if !ValidKind(c.Kind) {
		c.Kind = KindProject
	}
	c.Topic = NormalizeTopic(c.Topic)

	emb, err := model.Embed(embeddingText(c))
	if err != nil {
		return 0, err
	}
	now := s.now()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	id := c.ID
	if id > 0 {
		var weight float64
		var lastTouched int64
		err := tx.QueryRowContext(ctx, `SELECT weight, last_touched FROM memory_concepts WHERE id = ?`, id).Scan(&weight, &lastTouched)
		if errors.Is(err, sql.ErrNoRows) {
			id = 0
		} else if err != nil {
			return 0, err
		} else {
			weight = bumpWeight(decayedWeight(weight, now.Sub(time.Unix(lastTouched, 0)), ConceptHalfLife), ImplicitBump, false)
			if _, err := tx.ExecContext(ctx, `
				UPDATE memory_concepts SET scope = ?, kind = ?, topic = ?, title = ?, body = ?, weight = ?, last_touched = ?
				WHERE id = ?`, c.Scope, c.Kind, c.Topic, c.Title, c.Body, weight, now.Unix(), id); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM vec_concepts WHERE rowid = ?`, id); err != nil {
				return 0, err
			}
		}
	}
	if id == 0 {
		created := c.CreatedAt
		if created.IsZero() {
			created = now
		}
		touched := c.LastTouched
		if touched.IsZero() {
			touched = now
		}
		weight := c.Weight
		if weight <= 0 {
			weight = WeightInitial
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO memory_concepts (scope, kind, topic, title, body, weight, created_at, last_touched)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Scope, c.Kind, c.Topic, c.Title, c.Body, clampWeight(weight), created.Unix(), touched.Unix())
		if err != nil {
			return 0, err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO vec_concepts (rowid, embedding) VALUES (?, ?)`, id, SerializeVector(emb)); err != nil {
		return 0, err
	}
	if c.Topic != "" {
		if err := touchTopicTx(ctx, tx, c.Scope, c.Topic, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// Get returns one concept by id with its decayed weight.
func (s *Service) Get(ctx context.Context, id int64) (Concept, error) {
	db, _, err := s.handles()
	if err != nil {
		return Concept{}, err
	}
	row := db.QueryRowContext(ctx, `
		SELECT id, scope, kind, topic, title, body, weight, access_count, created_at, last_touched
		FROM memory_concepts WHERE id = ?`, id)
	c, err := scanConcept(row)
	if err != nil {
		return Concept{}, err
	}
	c.Weight = decayedWeight(c.Weight, s.now().Sub(c.LastTouched), ConceptHalfLife)
	return c, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConcept(r rowScanner) (Concept, error) {
	var c Concept
	var created, touched int64
	if err := r.Scan(&c.ID, &c.Scope, &c.Kind, &c.Topic, &c.Title, &c.Body, &c.Weight, &c.AccessCount, &created, &touched); err != nil {
		return Concept{}, err
	}
	c.CreatedAt = time.Unix(created, 0)
	c.LastTouched = time.Unix(touched, 0)
	return c, nil
}

func scopeFilter(cwd string) (string, []any) {
	scope := ScopeFor(cwd)
	if scope == "" {
		return "", nil
	}
	return " AND c.scope IN (?, ?)", []any{scope, ScopeGlobal}
}

// Recall embeds the query, pulls the nearest candidates in the project
// and global scopes, reranks them by similarity and decayed weight,
// infers the turn's topic from them, and bumps what it returns.
func (s *Service) Recall(ctx context.Context, q RecallQuery) (RecallResult, error) {
	db, model, err := s.handles()
	if err != nil {
		return RecallResult{}, err
	}
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" {
		return RecallResult{Topic: NormalizeTopic(q.Topic)}, nil
	}
	if q.K <= 0 {
		q.K = DefaultRecallK
	}
	emb, err := model.Embed(q.Query)
	if err != nil {
		return RecallResult{}, err
	}
	filter, args := scopeFilter(q.Cwd)
	query := `
		SELECT c.id, c.scope, c.kind, c.topic, c.title, c.body, c.weight, c.access_count, c.created_at, c.last_touched,
			vec_distance_cosine(v.embedding, ?) AS dist
		FROM vec_concepts v
		JOIN memory_concepts c ON c.id = v.rowid
		WHERE dist < ?` + filter + `
		ORDER BY dist ASC
		LIMIT ?`
	qargs := append([]any{SerializeVector(emb), MaxDistance}, args...)
	qargs = append(qargs, CandidateOversample)
	rows, err := db.QueryContext(ctx, query, qargs...)
	if err != nil {
		return RecallResult{}, err
	}
	now := s.now()
	var candidates []Concept
	for rows.Next() {
		var c Concept
		var created, touched int64
		if err := rows.Scan(&c.ID, &c.Scope, &c.Kind, &c.Topic, &c.Title, &c.Body, &c.Weight, &c.AccessCount, &created, &touched, &c.Distance); err != nil {
			rows.Close()
			return RecallResult{}, err
		}
		c.CreatedAt = time.Unix(created, 0)
		c.LastTouched = time.Unix(touched, 0)
		c.Weight = decayedWeight(c.Weight, now.Sub(c.LastTouched), ConceptHalfLife)
		c.Score = rerankScore(c.Distance, c.Weight)
		candidates = append(candidates, c)
	}
	rows.Close()

	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	topic := inferTopic(candidates, NormalizeTopic(q.Topic))
	if topic != "" {
		for i := range candidates {
			if candidates[i].Topic == topic {
				candidates[i].Score *= TopicBoost
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	}
	if len(candidates) > q.K {
		candidates = candidates[:q.K]
	}

	if !q.Silent && len(candidates) > 0 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return RecallResult{}, err
		}
		for i := range candidates {
			candidates[i].Weight = bumpWeight(candidates[i].Weight, ImplicitBump, false)
			candidates[i].AccessCount++
			candidates[i].LastTouched = now
			if _, err := tx.ExecContext(ctx, `
				UPDATE memory_concepts SET weight = ?, access_count = access_count + 1, last_touched = ? WHERE id = ?`,
				candidates[i].Weight, now.Unix(), candidates[i].ID); err != nil {
				tx.Rollback()
				return RecallResult{}, err
			}
		}
		if err := tx.Commit(); err != nil {
			return RecallResult{}, err
		}
	}
	return RecallResult{Concepts: candidates, Topic: topic}, nil
}

// inferTopic returns the topic shared by the most of the top-ranked
// candidates when at least two agree, else the caller's own topic.
func inferTopic(ranked []Concept, fallback string) string {
	counts := map[string]int{}
	limit := len(ranked)
	if limit > 10 {
		limit = 10
	}
	best, bestN := "", 0
	for _, c := range ranked[:limit] {
		if c.Topic == "" {
			continue
		}
		counts[c.Topic]++
		if counts[c.Topic] > bestN {
			best, bestN = c.Topic, counts[c.Topic]
		}
	}
	if bestN >= 2 {
		return best
	}
	return fallback
}

// Top returns the highest-weighted concepts in the project and global
// scopes. No embedding, no bump: this is the session-start block.
func (s *Service) Top(ctx context.Context, cwd string, k int) ([]Concept, error) {
	db, _, err := s.handles()
	if err != nil {
		return nil, err
	}
	if k <= 0 {
		k = DefaultRecallK
	}
	filter, args := scopeFilter(cwd)
	rows, err := db.QueryContext(ctx, `
		SELECT c.id, c.scope, c.kind, c.topic, c.title, c.body, c.weight, c.access_count, c.created_at, c.last_touched
		FROM memory_concepts c WHERE 1=1`+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.now()
	var out []Concept
	for rows.Next() {
		c, err := scanConcept(rows)
		if err != nil {
			return nil, err
		}
		c.Weight = decayedWeight(c.Weight, now.Sub(c.LastTouched), ConceptHalfLife)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].LastTouched.After(out[j].LastTouched)
	})
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// Reinforce applies a positive explicit bump. It reports false when the
// concept was explicitly touched inside RefractoryPeriod.
func (s *Service) Reinforce(ctx context.Context, id int64) (bool, error) {
	return s.explicitBump(ctx, id, ExplicitBump)
}

// Demote applies a negative explicit bump, clamped at WeightFloor. The
// concept is never deleted.
func (s *Service) Demote(ctx context.Context, id int64) (bool, error) {
	return s.explicitBump(ctx, id, -ExplicitBump)
}

func (s *Service) explicitBump(ctx context.Context, id int64, delta float64) (bool, error) {
	db, _, err := s.handles()
	if err != nil {
		return false, err
	}
	now := s.now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var weight float64
	var touched, explicit int64
	err = tx.QueryRowContext(ctx, `SELECT weight, last_touched, last_explicit FROM memory_concepts WHERE id = ?`, id).Scan(&weight, &touched, &explicit)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("no concept #%d", id)
	}
	if err != nil {
		return false, err
	}
	decayed := decayedWeight(weight, now.Sub(time.Unix(touched, 0)), ConceptHalfLife)
	applied := explicit == 0 || now.Sub(time.Unix(explicit, 0)) >= RefractoryPeriod
	next := decayed
	if applied {
		next = bumpWeight(decayed, delta, true)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_concepts SET weight = ?, last_touched = ?, last_explicit = ?, access_count = access_count + 1 WHERE id = ?`,
		next, now.Unix(), explicitStamp(applied, explicit, now), id); err != nil {
		return false, err
	}
	return applied, tx.Commit()
}

func explicitStamp(applied bool, prev int64, now time.Time) int64 {
	if applied {
		return now.Unix()
	}
	return prev
}

// Forget hard-deletes a concept and its vector.
func (s *Service) Forget(ctx context.Context, id int64) error {
	db, _, err := s.handles()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM memory_concepts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no concept #%d", id)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM vec_concepts WHERE rowid = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// NormalizeTopic lowercases, collapses whitespace, and caps a topic at
// three words so the taxonomy stays short.
func NormalizeTopic(t string) string {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(t)))
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.Join(words, " ")
}

func touchTopicTx(ctx context.Context, tx *sql.Tx, scope, name string, now time.Time) error {
	var weight float64
	var touched int64
	err := tx.QueryRowContext(ctx, `SELECT weight, last_touched FROM memory_topics WHERE scope = ? AND name = ?`, scope, name).Scan(&weight, &touched)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_topics (scope, name, weight, created_at, last_touched) VALUES (?, ?, ?, ?, ?)`,
			scope, name, WeightInitial, now.Unix(), now.Unix())
		return err
	}
	if err != nil {
		return err
	}
	weight = bumpWeight(decayedWeight(weight, now.Sub(time.Unix(touched, 0)), TopicHalfLife), ImplicitBump*2, false)
	_, err = tx.ExecContext(ctx, `UPDATE memory_topics SET weight = ?, last_touched = ? WHERE scope = ? AND name = ?`, weight, now.Unix(), scope, name)
	return err
}

// TouchTopic creates or bumps a topic in cwd's scope (global when cwd is
// empty).
func (s *Service) TouchTopic(ctx context.Context, cwd, name string) error {
	db, _, err := s.handles()
	if err != nil {
		return err
	}
	name = NormalizeTopic(name)
	if name == "" {
		return nil
	}
	scope := ScopeFor(cwd)
	if scope == "" {
		scope = ScopeGlobal
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := touchTopicTx(ctx, tx, scope, name, s.now()); err != nil {
		return err
	}
	return tx.Commit()
}

// Topics returns the live topics for cwd's scope plus global, strongest
// first, deduplicated by name.
func (s *Service) Topics(ctx context.Context, cwd string, k int) ([]Topic, error) {
	db, _, err := s.handles()
	if err != nil {
		return nil, err
	}
	if k <= 0 {
		k = DefaultTopicK
	}
	filter, args := scopeFilter(cwd)
	filter = strings.ReplaceAll(filter, "c.scope", "scope")
	rows, err := db.QueryContext(ctx, `SELECT id, scope, name, weight, last_touched FROM memory_topics WHERE 1=1`+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.now()
	var out []Topic
	for rows.Next() {
		var t Topic
		var touched int64
		if err := rows.Scan(&t.ID, &t.Scope, &t.Name, &t.Weight, &touched); err != nil {
			return nil, err
		}
		t.LastTouched = time.Unix(touched, 0)
		t.Weight = decayedWeight(t.Weight, now.Sub(t.LastTouched), TopicHalfLife)
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	seen := map[string]bool{}
	deduped := out[:0]
	for _, t := range out {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		deduped = append(deduped, t)
	}
	if len(deduped) > k {
		deduped = deduped[:k]
	}
	return deduped, nil
}

// TopicNames is Topics reduced to names.
func (s *Service) TopicNames(ctx context.Context, cwd string, k int) []string {
	topics, err := s.Topics(ctx, cwd, k)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(topics))
	for _, t := range topics {
		names = append(names, t.Name)
	}
	return names
}

// AddSessionToMemory hands the session's last exchange to the installed
// Extractor. ADK calls this with a finished session; ask's own turn
// boundaries enqueue directly.
func (s *Service) AddSessionToMemory(ctx context.Context, sess session.Session) error {
	if s == nil || !s.IsOpen() {
		return errors.New("memory service closed")
	}
	if sess == nil {
		return nil
	}
	s.mu.RLock()
	extractor := s.extractor
	s.mu.RUnlock()
	if extractor == nil {
		return errors.New("memory extractor not configured")
	}
	rec := TurnFromSession(sess)
	if rec.Prompt == "" || rec.Response == "" {
		return nil
	}
	extractor.Enqueue(rec)
	return nil
}

// TurnFromSession reduces a session to its last exchange: the final user
// text and every model text after it. Cwd comes from session state when
// a caller stored one.
func TurnFromSession(sess session.Session) TurnRecord {
	rec := TurnRecord{}
	if sess == nil {
		return rec
	}
	if state := sess.State(); state != nil {
		if val, err := state.Get("cwd"); err == nil {
			if str, ok := val.(string); ok {
				rec.Cwd = str
			}
		}
	}
	events := sess.Events()
	if events == nil {
		return rec
	}
	var responses []string
	for event := range events.All() {
		if event == nil || event.Content == nil {
			continue
		}
		var textParts []string
		for _, part := range event.Content.Parts {
			if part == nil || part.Thought || part.Text == "" {
				continue
			}
			textParts = append(textParts, part.Text)
		}
		text := strings.TrimSpace(strings.Join(textParts, "\n"))
		if text == "" {
			continue
		}
		if event.Content.Role == genai.RoleUser {
			rec.Prompt = text
			responses = responses[:0]
			continue
		}
		responses = append(responses, text)
	}
	rec.Response = strings.Join(responses, "\n")
	return rec
}

// SearchMemory is the ADK-shaped recall: every scope, no topic.
func (s *Service) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	if s == nil || !s.IsOpen() {
		return &adkmemory.SearchResponse{}, nil
	}
	if req == nil || strings.TrimSpace(req.Query) == "" {
		return &adkmemory.SearchResponse{}, nil
	}
	res, err := s.Recall(ctx, RecallQuery{Query: req.Query, K: DefaultRecallK})
	if err != nil {
		return nil, err
	}
	entries := make([]adkmemory.Entry, 0, len(res.Concepts))
	for _, c := range res.Concepts {
		entries = append(entries, adkmemory.Entry{
			ID:        fmt.Sprintf("%d", c.ID),
			Content:   genai.NewContentFromText(c.Title+"\n"+c.Body, genai.RoleModel),
			Timestamp: c.LastTouched,
			CustomMetadata: map[string]any{
				"scope":    c.Scope,
				"kind":     c.Kind,
				"topic":    c.Topic,
				"weight":   c.Weight,
				"distance": c.Distance,
			},
		})
	}
	return &adkmemory.SearchResponse{Memories: entries}, nil
}

// Package-level singleton accessors

// Open initializes the default process-wide memory service.
func Open(opts Options) error {
	defaultMu.Lock()
	defer defaultMu.Unlock()

	if defaultSvc != nil && defaultSvc.IsOpen() {
		return nil
	}

	svc, err := NewService(opts)
	if err != nil {
		return err
	}
	defaultSvc = svc
	return nil
}

// Close tears down the default process-wide memory service.
func Close() error {
	defaultMu.Lock()
	defer defaultMu.Unlock()

	if defaultSvc != nil {
		err := defaultSvc.Close()
		defaultSvc = nil
		return err
	}
	return nil
}

// IsOpen reports whether the default process-wide memory service is open.
func IsOpen() bool {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultSvc != nil && defaultSvc.IsOpen()
}

// Default returns the default global Service instance.
func Default() *Service {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultSvc
}

// SetDefault replaces the default global Service instance.
func SetDefault(s *Service) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultSvc = s
}

// Upsert stores a concept in the default service.
func Upsert(ctx context.Context, c Concept) (int64, error) {
	return Default().Upsert(ctx, c)
}

// Recall queries the default service.
func Recall(ctx context.Context, q RecallQuery) (RecallResult, error) {
	return Default().Recall(ctx, q)
}
