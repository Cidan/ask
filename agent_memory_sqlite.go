package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	_ "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

var (
	memDB    *sql.DB
	memModel *EmbeddingModel
	memMu    sync.RWMutex
)

type memoryRecallHit struct {
	ID         int64
	ProjectID  string
	Text       string
	Distance   float32
	LastRecall time.Time
}

// openMemoryService initializes the SQLite database and the embedding model.
func openMemoryService(cfg askConfig) error {
	memMu.Lock()
	defer memMu.Unlock()

	if memDB != nil {
		return nil
	}

	modelPath := filepath.Join(projectRoot("."), "build", "models", "embeddinggemma-300M-Q8_0.gguf")
	if !filepath.IsAbs(modelPath) {
		cwd, _ := os.Getwd()
		modelPath = filepath.Join(projectRoot(cwd), "build", "models", "embeddinggemma-300M-Q8_0.gguf")
	}

	model, err := LoadEmbeddingModel(modelPath)
	if err != nil {
		return fmt.Errorf("failed to load embedding model: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		model.Close()
		return err
	}
	dbDir := filepath.Join(home, ".config", "ask", "memory")
	if err := os.MkdirAll(dbDir, 0700); err != nil {
		model.Close()
		return err
	}
	dbPath := filepath.Join(dbDir, "memory.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		model.Close()
		return fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Schema
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
		model.Close()
		return err
	}

	embdSize := model.EmbdSize()
	_, err = db.Exec(fmt.Sprintf(`
		CREATE VIRTUAL TABLE IF NOT EXISTS vec_memory USING vec0(
			embedding float[%d]
		);
	`, embdSize))
	if err != nil {
		db.Close()
		model.Close()
		return err
	}

	memDB = db
	memModel = model

	// Start background sweeper
	go sweepOldMemories()

	return nil
}

func closeMemoryService() error {
	memMu.Lock()
	defer memMu.Unlock()

	if memModel != nil {
		memModel.Close()
		memModel = nil
	}
	if memDB != nil {
		err := memDB.Close()
		memDB = nil
		return err
	}
	return nil
}

func memoryServiceOpen() bool {
	memMu.RLock()
	defer memMu.RUnlock()
	return memDB != nil && memModel != nil
}

func sweepOldMemories() {
	memMu.RLock()
	db := memDB
	memMu.RUnlock()

	if db == nil {
		return
	}

	// Delete older than 30 days
	_, err := db.Exec(`
		DELETE FROM vec_memory WHERE rowid IN (
			SELECT id FROM project_memory WHERE last_recalled_at < datetime('now', '-30 days')
		);
	`)
	if err != nil {
		debugLog("sweep vec_memory err: %v", err)
	}

	_, err = db.Exec(`
		DELETE FROM project_memory WHERE last_recalled_at < datetime('now', '-30 days');
	`)
	if err != nil {
		debugLog("sweep project_memory err: %v", err)
	}
}

func serializeVector(vec []float32) []byte {
	b, _ := json.Marshal(vec)
	return b
}

func memoryIndex(ctx context.Context, cwd, text string) error {
	memMu.RLock()
	db := memDB
	model := memModel
	memMu.RUnlock()

	if db == nil || model == nil {
		return errors.New("memory service closed")
	}

	emb, err := model.Embed(text)
	if err != nil {
		return err
	}

	pid := projectKey(cwd)

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

	_, err = tx.ExecContext(ctx, "INSERT INTO vec_memory (rowid, embedding) VALUES (?, ?)", id, serializeVector(emb))
	if err != nil {
		return err
	}

	return tx.Commit()
}

func memoryRecall(ctx context.Context, cwd, prompt string, k int) ([]memoryRecallHit, error) {
	memMu.RLock()
	db := memDB
	model := memModel
	memMu.RUnlock()

	if db == nil || model == nil {
		return nil, errors.New("memory service closed")
	}

	emb, err := model.Embed(prompt)
	if err != nil {
		return nil, err
	}

	pid := projectKey(cwd)

	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.project_id, p.text_payload, p.last_recalled_at, vec_distance_cosine(v.embedding, ?) as dist
		FROM vec_memory v
		JOIN project_memory p ON p.id = v.rowid
		WHERE p.project_id = ? AND dist < 0.4
		ORDER BY dist ASC
		LIMIT ?
	`, serializeVector(emb), pid, k)
	
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []memoryRecallHit
	var ids []int64

	for rows.Next() {
		var h memoryRecallHit
		if err := rows.Scan(&h.ID, &h.ProjectID, &h.Text, &h.LastRecall, &h.Distance); err != nil {
			return nil, err
		}
		hits = append(hits, h)
		ids = append(ids, h.ID)
	}

	if len(ids) > 0 {
		// Update last_recalled_at
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

// memoryAwareTool is the PreToolUse twin: it decorates a file tool
// (read / edit / write) so the result carries a clearly-delimited
// memory footer for the touched path — prior work on the same file
// lands in context exactly when needed.
type memoryAwareTool struct {
	fantasy.AgentTool
	cwd string
}

// wrapFileToolsWithMemory decorates the file tools in place. Cheap
// when memory is closed: the wrapper checks per call.
func wrapFileToolsWithMemory(tools []fantasy.AgentTool, cwd string) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(tools))
	for i, t := range tools {
		switch t.Info().Name {
		case "read", "edit", "write":
			out[i] = &memoryAwareTool{AgentTool: t, cwd: cwd}
		default:
			out[i] = t
		}
	}
	return out
}

func (m *memoryAwareTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := m.AgentTool.Run(ctx, call)
	if err != nil || resp.IsError || resp.Type != "text" || !memoryServiceOpen() {
		return resp, err
	}
	path := fileToolPath(call.Input)
	if path == "" {
		return resp, err
	}
	recallCtx, cancel := context.WithTimeout(ctx, memoryHookCtxTimeout)
	defer cancel()
	hits, rerr := memoryRecall(recallCtx, m.cwd, path, memoryRecallK)
	if rerr != nil {
		debugLog("agent memory (file %s): %v", path, rerr)
		return resp, err
	}
	if block := formatRecallContext(hits, "Memory for "+path); block != "" {
		resp.Content = resp.Content + "\n\n" + block
	}
	return resp, err
}

// fileToolPath pulls file_path out of a file tool's JSON input.
func fileToolPath(input string) string {
	var in struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return ""
	}
	return strings.TrimSpace(in.FilePath)
}

// memoryRecallK caps the number of nodes injected into any single
// hook response.
const memoryRecallK = 5

// memoryHookCtxTimeout caps how long a hook can spend talking to
// the embedder.
const memoryHookCtxTimeout = 8 * time.Second

func formatRecallContext(hits []memoryRecallHit, heading string) string {
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		text := strings.TrimSpace(h.Text)
		if text == "" {
			continue
		}
		lines = append(lines, text)
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", heading)
	for i, text := range lines {
		fmt.Fprintf(&b, "%d. %s\n", i+1, text)
	}
	return strings.TrimRight(b.String(), "\n")
}

func agentMemorySystemBlock(cwd string) string {
	if !memoryServiceOpen() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), memoryHookCtxTimeout)
	defer cancel()
	hits, err := memoryRecall(ctx, cwd, "current project context", memoryRecallK)
	if err != nil {
		debugLog("agent memory (session start): %v", err)
		return ""
	}
	return formatRecallContext(hits, "Project memory")
}

func agentMemoryPromptContext(cwd, prompt string) string {
	if !memoryServiceOpen() || strings.TrimSpace(prompt) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), memoryHookCtxTimeout)
	defer cancel()
	hits, err := memoryRecall(ctx, cwd, prompt, memoryRecallK)
	if err != nil {
		debugLog("agent memory (prompt): %v", err)
		return ""
	}
	return formatRecallContext(hits, "Relevant memory")
}
