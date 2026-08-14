package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Cidan/ask/pkg/config"
	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

var (
	defaultMu  sync.RWMutex
	defaultSvc *Service
)

// RecallHit represents a matched memory entry from vector search.
type RecallHit struct {
	ID         int64     `json:"id"`
	ProjectID  string    `json:"project_id"`
	Text       string    `json:"text"`
	Distance   float32   `json:"distance"`
	LastRecall time.Time `json:"last_recall"`
}

// Options configures the initialization of the Memory Service.
type Options struct {
	DBPath    string
	ModelPath string
	Embedder  Embedder
}

// Service manages vector memory persistence and retrieval.
type Service struct {
	db       *sql.DB
	embedder Embedder
	mu       sync.RWMutex
}

// DefaultDBPath returns the standard SQLite vector database file location.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "memory", "memory.db"), nil
}

// DefaultModelPath returns the standard local embedding model GGUF location.
func DefaultModelPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "ask", "models", "embeddinggemma-300M-Q8_0.gguf"), nil
}

// NewService initializes a SQLite-vec memory service with the specified options.
func NewService(opts Options) (*Service, error) {
	var embedder Embedder
	if opts.Embedder != nil {
		embedder = opts.Embedder
	} else {
		modelPath := opts.ModelPath
		if modelPath == "" {
			var err error
			modelPath, err = DefaultModelPath()
			if err != nil {
				return nil, err
			}
		}
		loaded, err := LoadEmbeddingModel(modelPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load embedding model: %w", err)
		}
		embedder = loaded
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		var err error
		dbPath, err = DefaultDBPath()
		if err != nil {
			embedder.Close()
			return nil, err
		}
	}

	dbDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dbDir, 0700); err != nil {
		embedder.Close()
		return nil, err
	}

	sqlite_vec.Auto()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		embedder.Close()
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Create tables
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS project_memory (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id TEXT,
			text_payload TEXT,
			last_recalled_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		db.Close()
		embedder.Close()
		return nil, err
	}

	embdSize := embedder.EmbdSize()
	_, err = db.Exec(fmt.Sprintf(`
		CREATE VIRTUAL TABLE IF NOT EXISTS vec_memory USING vec0(
			embedding float[%d]
		);
	`, embdSize))
	if err != nil {
		db.Close()
		embedder.Close()
		return nil, err
	}

	svc := &Service{
		db:       db,
		embedder: embedder,
	}

	// Run initial sweep in background
	go func() {
		_ = svc.Sweep(context.Background())
	}()

	return svc, nil
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

// SerializeVector converts a float32 slice to the byte representation expected by sqlite-vec.
func SerializeVector(vec []float32) []byte {
	b, _ := sqlite_vec.SerializeFloat32(vec)
	return b
}

// Index embeds and saves a text memory associated with the given working directory.
func (s *Service) Index(ctx context.Context, cwd, text string) error {
	if s == nil {
		return errors.New("memory service closed")
	}
	s.mu.RLock()
	db := s.db
	model := s.embedder
	s.mu.RUnlock()

	if db == nil || model == nil {
		return errors.New("memory service closed")
	}

	emb, err := model.Embed(text)
	if err != nil {
		return err
	}

	pid := config.ProjectRoot(cwd)
	if pid == "" {
		pid = cwd
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, "INSERT INTO project_memory (project_id, text_payload) VALUES (?, ?)", pid, text)
	if err != nil {
		return err
	}

	id, err := res.LastInsertId()
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, "INSERT INTO vec_memory (rowid, embedding) VALUES (?, ?)", id, SerializeVector(emb))
	if err != nil {
		return err
	}

	return tx.Commit()
}

// Recall performs vector cosine similarity search for the given prompt in the project context.
func (s *Service) Recall(ctx context.Context, cwd, prompt string, k int) ([]RecallHit, error) {
	if s == nil {
		return nil, errors.New("memory service closed")
	}
	s.mu.RLock()
	db := s.db
	model := s.embedder
	s.mu.RUnlock()

	if db == nil || model == nil {
		return nil, errors.New("memory service closed")
	}

	emb, err := model.Embed(prompt)
	if err != nil {
		return nil, err
	}

	pid := config.ProjectRoot(cwd)
	if pid == "" {
		pid = cwd
	}

	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.project_id, p.text_payload, p.last_recalled_at, vec_distance_cosine(v.embedding, ?) as dist
		FROM vec_memory v
		JOIN project_memory p ON p.id = v.rowid
		WHERE p.project_id = ? AND dist < 0.4
		ORDER BY dist ASC
		LIMIT ?
	`, SerializeVector(emb), pid, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []RecallHit
	var ids []int64

	for rows.Next() {
		var h RecallHit
		if err := rows.Scan(&h.ID, &h.ProjectID, &h.Text, &h.LastRecall, &h.Distance); err != nil {
			return nil, err
		}
		hits = append(hits, h)
		ids = append(ids, h.ID)
	}

	if len(ids) > 0 {
		args := make([]any, len(ids))
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			args[i] = id
			placeholders[i] = "?"
		}
		_, _ = db.ExecContext(ctx, fmt.Sprintf("UPDATE project_memory SET last_recalled_at = CURRENT_TIMESTAMP WHERE id IN (%s)", strings.Join(placeholders, ",")), args...)
	}

	return hits, nil
}

// Sweep prunes memory records older than 30 days.
func (s *Service) Sweep(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()

	if db == nil {
		return nil
	}

	_, err := db.ExecContext(ctx, `
		DELETE FROM vec_memory WHERE rowid IN (
			SELECT id FROM project_memory WHERE last_recalled_at < datetime('now', '-30 days')
		);
	`)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
		DELETE FROM project_memory WHERE last_recalled_at < datetime('now', '-30 days');
	`)
	return err
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

// Index embeds and saves a text memory using the default service.
func Index(ctx context.Context, cwd, text string) error {
	defaultMu.RLock()
	svc := defaultSvc
	defaultMu.RUnlock()
	if svc == nil {
		return errors.New("memory service closed")
	}
	return svc.Index(ctx, cwd, text)
}

// Recall queries the default memory service for relevant hits.
func Recall(ctx context.Context, cwd, prompt string, k int) ([]RecallHit, error) {
	defaultMu.RLock()
	svc := defaultSvc
	defaultMu.RUnlock()
	if svc == nil {
		return nil, errors.New("memory service closed")
	}
	return svc.Recall(ctx, cwd, prompt, k)
}

// Sweep triggers maintenance on the default service.
func Sweep(ctx context.Context) error {
	defaultMu.RLock()
	svc := defaultSvc
	defaultMu.RUnlock()
	if svc == nil {
		return nil
	}
	return svc.Sweep(ctx)
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
