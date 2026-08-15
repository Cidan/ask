package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

// SessionFile represents the serialized on-disk format for a session transcript.
type SessionFile struct {
	Version   int               `json:"version"`
	Cwd       string            `json:"cwd"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	Messages  []fantasy.Message `json:"messages"`
}

// SessionSummary summarizes a stored session for listing.
type SessionSummary struct {
	ID      string    `json:"id"`
	Cwd     string    `json:"cwd"`
	Preview string    `json:"preview"`
	ModTime time.Time `json:"modTime"`
}

// SessionStore manages persisting and restoring agent session transcripts
// under ~/.config/ask/agent-sessions/<provider>/<encoded-cwd>/<id>.json.
type SessionStore struct {
	provider string
}

// NewSessionStore creates a new SessionStore for the given provider ID.
func NewSessionStore(provider string) *SessionStore {
	return &SessionStore{provider: provider}
}

// Root returns the base directory for stored sessions for this provider.
func (st *SessionStore) Root() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "ask", "agent-sessions", st.provider), nil
}

// DirFor returns the directory path for sessions stored for a specific working directory.
func (st *SessionStore) DirFor(cwd string) (string, error) {
	root, err := st.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, EncodeProjectDir(cwd)), nil
}

// PathFor locates an existing session file by ID across all project directories.
func (st *SessionStore) PathFor(id string) (string, error) {
	root, err := st.Root()
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", id+".json"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no stored session found for id %s", id)
	}
	return matches[0], nil
}

// Save writes the full transcript atomically with 0600 permissions.
// CreatedAt timestamp is preserved across subsequent saves.
func (st *SessionStore) Save(id, cwd string, messages []fantasy.Message) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("cannot save session with empty id")
	}
	dir, err := st.DirFor(cwd)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, id+".json")
	now := time.Now()
	file := SessionFile{
		Version:   1,
		Cwd:       cwd,
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  messages,
	}
	if prev, err := ReadSessionFile(path); err == nil && !prev.CreatedAt.IsZero() {
		file.CreatedAt = prev.CreatedAt
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+id+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// Load reads and parses a stored session file by session ID.
func (st *SessionStore) Load(id string) (SessionFile, error) {
	path, err := st.PathFor(id)
	if err != nil {
		return SessionFile{}, err
	}
	return ReadSessionFile(path)
}

// Delete removes a stored session file by session ID.
func (st *SessionStore) Delete(id string) error {
	path, err := st.PathFor(id)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// List enumerates sessions stored for a specific working directory, newest first.
func (st *SessionStore) List(cwd string) ([]SessionSummary, error) {
	dir, err := st.DirFor(cwd)
	if err != nil {
		return nil, err
	}
	dirents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []SessionSummary
	for _, e := range dirents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := ReadSessionFile(path)
		if err != nil {
			continue
		}
		sessions = append(sessions, SessionSummary{
			ID:      strings.TrimSuffix(e.Name(), ".json"),
			Cwd:     file.Cwd,
			Preview: SessionPreview(file.Messages),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})
	return sessions, nil
}

// ReadSessionFile reads and parses a session JSON file from disk.
func ReadSessionFile(path string) (SessionFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionFile{}, err
	}
	var file SessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return SessionFile{}, err
	}
	if file.Version != 1 {
		return SessionFile{}, fmt.Errorf("unsupported session version %d", file.Version)
	}
	return file, nil
}

// SessionPreview extracts the first user prompt line to serve as a preview.
func SessionPreview(messages []fantasy.Message) string {
	for _, m := range messages {
		if m.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				line := strings.TrimSpace(strings.SplitN(tp.Text, "\n", 2)[0])
				if len(line) > 80 {
					line = line[:80] + "…"
				}
				if line != "" {
					return line
				}
			}
		}
	}
	return "(empty session)"
}

var projectDirNonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

const projectDirMaxLen = 200

// EncodeProjectDir encodes a path into a filesystem-safe directory name.
func EncodeProjectDir(path string) string {
	enc := projectDirNonAlnum.ReplaceAllString(path, "-")
	if len(enc) <= projectDirMaxLen {
		return enc
	}
	return enc[:projectDirMaxLen] + "-" + projectDirHash(path)
}

func projectDirHash(path string) string {
	var h int32
	for _, r := range path {
		h = h*31 + r
	}
	n := int64(h)
	if n < 0 {
		n = -n
	}
	return strconv.FormatInt(n, 36)
}
