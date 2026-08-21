package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// StoredSessionFile represents the serialized on-disk format for a session transcript and state.
type StoredSessionFile struct {
	Version   int              `json:"version"`
	AppName   string           `json:"appName"`
	UserID    string           `json:"userID"`
	SessionID string           `json:"sessionID"`
	Cwd       string           `json:"cwd"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
	State     map[string]any   `json:"state,omitempty"`
	Events    []*session.Event `json:"events"`
}

// Messages converts stored Events into native ask Message slices.
func (f StoredSessionFile) Messages() []Message {
	return MessagesFromEvents(f.Events)
}

// MessagesFromEvents converts ADK session.Event slices to native ask Message slices.
func MessagesFromEvents(events []*session.Event) []Message {
	var msgs []Message
	for _, e := range events {
		if e == nil {
			continue
		}
		if e.LLMResponse.Content != nil {
			msg := MessageFromGenAIContent(e.LLMResponse.Content)
			if e.Author == "user" && msg.Role != RoleUser {
				msg.Role = RoleUser
			} else if (e.Author == "ask_coder" || e.Author == "model" || e.Author == "assistant") && msg.Role != RoleAssistant && msg.Role != RoleModel {
				msg.Role = RoleAssistant
			}
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

// SessionSummary summarizes a stored session for listing.
type SessionSummary struct {
	ID      string    `json:"id"`
	Cwd     string    `json:"cwd"`
	Preview string    `json:"preview"`
	ModTime time.Time `json:"modTime"`
}

// FileSessionService implements google.golang.org/adk/v2/session.Service
// backed by atomic JSON files under ~/.config/ask/agent-sessions/<provider>/<encoded-cwd>/<sessionID>.json.
type FileSessionService struct {
	provider string
	cwd      string
	baseDir  string // Optional base directory override for tests

	mu        sync.RWMutex
	appState  map[string]map[string]any            // appName -> key -> val
	userState map[string]map[string]map[string]any // appName -> userID -> key -> val
}

// NewFileSessionService creates a new FileSessionService for the given provider and working directory.
func NewFileSessionService(provider, cwd string) *FileSessionService {
	return &FileSessionService{
		provider:  provider,
		cwd:       cwd,
		appState:  make(map[string]map[string]any),
		userState: make(map[string]map[string]map[string]any),
	}
}

// NewFileSessionServiceWithBaseDir creates a FileSessionService with a custom base directory (useful for testing).
func NewFileSessionServiceWithBaseDir(provider, cwd, baseDir string) *FileSessionService {
	return &FileSessionService{
		provider:  provider,
		cwd:       cwd,
		baseDir:   baseDir,
		appState:  make(map[string]map[string]any),
		userState: make(map[string]map[string]map[string]any),
	}
}

// Root returns the base directory for stored sessions for this provider.
func (s *FileSessionService) Root() (string, error) {
	if s.baseDir != "" {
		if s.provider != "" {
			return filepath.Join(s.baseDir, s.provider), nil
		}
		return s.baseDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	provider := s.provider
	if provider == "" {
		provider = "vertex"
	}
	return filepath.Join(home, ".config", "ask", "agent-sessions", provider), nil
}

// DirFor returns the directory path for sessions stored for a specific working directory.
func (s *FileSessionService) DirFor(cwd string) (string, error) {
	root, err := s.Root()
	if err != nil {
		return "", err
	}
	if cwd == "" {
		cwd = s.cwd
	}
	if cwd == "" {
		cwd = "."
	}
	return filepath.Join(root, EncodeProjectDir(cwd)), nil
}

// PathFor locates an existing session file by ID across all project directories.
func (s *FileSessionService) PathFor(id string) (string, error) {
	root, err := s.Root()
	if err != nil {
		return "", err
	}
	if s.cwd != "" {
		p := filepath.Join(root, EncodeProjectDir(s.cwd), id+".json")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", id+".json"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("session %s not found", id)
	}
	return matches[0], nil
}

// Create initializes a new session and persists it atomically to disk.
func (s *FileSessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req == nil || req.AppName == "" || req.UserID == "" {
		appName, userID := "", ""
		if req != nil {
			appName = req.AppName
			userID = req.UserID
		}
		return nil, fmt.Errorf("app_name and user_id are required, got app_name: %q, user_id: %q", appName, userID)
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.PathFor(sessionID); err == nil {
		return nil, fmt.Errorf("session %s already exists", sessionID)
	}

	cwd := s.cwd
	if cwd == "" {
		cwd = "."
	}
	dir, err := s.DirFor(cwd)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	filePath := filepath.Join(dir, sessionID+".json")

	now := time.Now()
	appDelta, userDelta, sessionState := extractStateDeltas(req.State)
	s.updateAppStateLocked(appDelta, req.AppName)
	s.updateUserStateLocked(userDelta, req.AppName, req.UserID)

	file := StoredSessionFile{
		Version:   1,
		AppName:   req.AppName,
		UserID:    req.UserID,
		SessionID: sessionID,
		Cwd:       cwd,
		CreatedAt: now,
		UpdatedAt: now,
		State:     sessionState,
		Events:    make([]*session.Event, 0),
	}

	if err := saveStoredSessionFileAtomic(filePath, &file); err != nil {
		return nil, err
	}

	mergedState := s.mergeStatesLocked(sessionState, req.AppName, req.UserID)
	sess := newFileSession(file.AppName, file.UserID, file.SessionID, file.Cwd, file.UpdatedAt, mergedState, file.Events, s)
	return &session.CreateResponse{Session: sess}, nil
}

// Get loads a session from disk, applying optional timestamp and event count filters.
func (s *FileSessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if req == nil || req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		appName, userID, sessionID := "", "", ""
		if req != nil {
			appName = req.AppName
			userID = req.UserID
			sessionID = req.SessionID
		}
		return nil, fmt.Errorf("app_name, user_id, session_id are required, got app_name: %q, user_id: %q, session_id: %q", appName, userID, sessionID)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	filePath, err := s.PathFor(req.SessionID)
	if err != nil {
		return nil, fmt.Errorf("session %s not found: %w", req.SessionID, err)
	}
	file, err := ReadStoredSessionFile(filePath)
	if err != nil {
		return nil, err
	}

	if file.AppName != "" && file.AppName != req.AppName {
		return nil, fmt.Errorf("session %s has app %q, requested %q", req.SessionID, file.AppName, req.AppName)
	}
	if file.UserID != "" && file.UserID != req.UserID {
		return nil, fmt.Errorf("session %s belongs to user %q, requested %q", req.SessionID, file.UserID, req.UserID)
	}

	filteredEvents := file.Events
	if req.NumRecentEvents > 0 {
		start := max(len(filteredEvents)-req.NumRecentEvents, 0)
		filteredEvents = filteredEvents[start:]
	}
	if !req.After.IsZero() && len(filteredEvents) > 0 {
		firstIdx := sort.Search(len(filteredEvents), func(i int) bool {
			return !filteredEvents[i].Timestamp.Before(req.After)
		})
		filteredEvents = filteredEvents[firstIdx:]
	}

	mergedState := s.mergeStatesLocked(file.State, req.AppName, req.UserID)
	sess := newFileSession(file.AppName, file.UserID, file.SessionID, file.Cwd, file.UpdatedAt, mergedState, filteredEvents, s)
	return &session.GetResponse{Session: sess}, nil
}

// List enumerates sessions, filtered by AppName and UserID (if provided).
func (s *FileSessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if req == nil || req.AppName == "" {
		appName := ""
		if req != nil {
			appName = req.AppName
		}
		return nil, fmt.Errorf("app_name is required, got app_name: %q", appName)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	root, err := s.Root()
	if err != nil {
		return nil, err
	}

	var pattern string
	if s.cwd != "" {
		pattern = filepath.Join(root, EncodeProjectDir(s.cwd), "*.json")
	} else {
		pattern = filepath.Join(root, "*", "*.json")
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	var sessions []session.Session
	for _, p := range matches {
		if strings.HasPrefix(filepath.Base(p), ".") {
			continue
		}
		file, err := ReadStoredSessionFile(p)
		if err != nil {
			continue
		}
		if file.AppName != "" && file.AppName != req.AppName {
			continue
		}
		if req.UserID != "" && file.UserID != "" && file.UserID != req.UserID {
			continue
		}
		mergedState := s.mergeStatesLocked(file.State, req.AppName, file.UserID)
		sess := newFileSession(file.AppName, file.UserID, file.SessionID, file.Cwd, file.UpdatedAt, mergedState, file.Events, s)
		sessions = append(sessions, sess)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastUpdateTime().After(sessions[j].LastUpdateTime())
	})

	return &session.ListResponse{Sessions: sessions}, nil
}

// Delete removes a session file from disk.
func (s *FileSessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if req == nil || req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		appName, userID, sessionID := "", "", ""
		if req != nil {
			appName = req.AppName
			userID = req.UserID
			sessionID = req.SessionID
		}
		return fmt.Errorf("app_name, user_id, session_id are required, got app_name: %q, user_id: %q, session_id: %q", appName, userID, sessionID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filePath, err := s.PathFor(req.SessionID)
	if err != nil {
		return nil
	}
	return os.Remove(filePath)
}

// AppendEvent appends a non-partial event, updates state deltas, and saves to disk atomically.
func (s *FileSessionService) AppendEvent(ctx context.Context, curSession session.Session, event *session.Event) error {
	if curSession == nil {
		return errors.New("session is nil")
	}
	if event == nil {
		return errors.New("event is nil")
	}
	if event.Partial {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	filePath, err := s.PathFor(curSession.ID())
	if err != nil {
		return fmt.Errorf("session %s not found, cannot apply event: %w", curSession.ID(), err)
	}
	file, err := ReadStoredSessionFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read session file: %w", err)
	}

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	if event.Author == "" {
		if event.LLMResponse.Content != nil && event.LLMResponse.Content.Role == genai.RoleUser {
			event.Author = "user"
		} else if event.LLMResponse.Content != nil && event.LLMResponse.Content.Role == genai.RoleModel {
			event.Author = "ask_coder"
		} else {
			event.Author = "user"
		}
	}

	eventCopy := cloneEvent(event)
	trimmedEvent := trimTempDeltaState(eventCopy)

	if len(event.Actions.StateDelta) > 0 {
		appDelta, userDelta, sessionDelta := extractStateDeltas(event.Actions.StateDelta)
		s.updateAppStateLocked(appDelta, curSession.AppName())
		s.updateUserStateLocked(userDelta, curSession.AppName(), curSession.UserID())
		if file.State == nil {
			file.State = make(map[string]any)
		}
		maps.Copy(file.State, sessionDelta)
	}

	file.Events = append(file.Events, trimmedEvent)
	file.UpdatedAt = event.Timestamp
	if file.CreatedAt.IsZero() {
		file.CreatedAt = event.Timestamp
	}

	if err := saveStoredSessionFileAtomic(filePath, &file); err != nil {
		return fmt.Errorf("failed to save session: %w", err)
	}

	if fs, ok := curSession.(*fileSession); ok {
		fs.mu.Lock()
		fs.events = append(fs.events, trimmedEvent)
		fs.updatedAt = event.Timestamp
		if len(event.Actions.StateDelta) > 0 {
			if fs.state != nil {
				_, _, sessionDelta := extractStateDeltas(event.Actions.StateDelta)
				maps.Copy(fs.state.sessionState, sessionDelta)
				fs.state.recomputeMerged(s, fs.appName, fs.userID)
			}
		}
		fs.mu.Unlock()
	}

	return nil
}

func (s *FileSessionService) updateAppStateLocked(appDelta map[string]any, appName string) {
	if len(appDelta) == 0 {
		return
	}
	inner, ok := s.appState[appName]
	if !ok {
		inner = make(map[string]any)
		s.appState[appName] = inner
	}
	maps.Copy(inner, appDelta)
}

func (s *FileSessionService) updateUserStateLocked(userDelta map[string]any, appName, userID string) {
	if len(userDelta) == 0 {
		return
	}
	innerUsers, ok := s.userState[appName]
	if !ok {
		innerUsers = make(map[string]map[string]any)
		s.userState[appName] = innerUsers
	}
	inner, ok := innerUsers[userID]
	if !ok {
		inner = make(map[string]any)
		innerUsers[userID] = inner
	}
	maps.Copy(inner, userDelta)
}

func (s *FileSessionService) mergeStatesLocked(sessionState map[string]any, appName, userID string) map[string]any {
	merged := make(map[string]any)
	if appMap, ok := s.appState[appName]; ok {
		maps.Copy(merged, appMap)
	}
	if userAppMap, ok := s.userState[appName]; ok {
		if userMap, ok := userAppMap[userID]; ok {
			maps.Copy(merged, userMap)
		}
	}
	if sessionState != nil {
		maps.Copy(merged, sessionState)
	}
	return merged
}

var _ session.Service = (*FileSessionService)(nil)

type fileSession struct {
	mu        sync.RWMutex
	appName   string
	userID    string
	sessionID string
	cwd       string
	updatedAt time.Time
	state     *fileSessionState
	events    []*session.Event
	svc       *FileSessionService
}

func newFileSession(appName, userID, sessionID, cwd string, updatedAt time.Time, mergedState map[string]any, events []*session.Event, svc *FileSessionService) *fileSession {
	fs := &fileSession{
		appName:   appName,
		userID:    userID,
		sessionID: sessionID,
		cwd:       cwd,
		updatedAt: updatedAt,
		events:    slices.Clone(events),
		svc:       svc,
	}
	fs.state = &fileSessionState{
		fs:           fs,
		sessionState: make(map[string]any),
		merged:       maps.Clone(mergedState),
	}
	return fs
}

func (s *fileSession) ID() string           { return s.sessionID }
func (s *fileSession) AppName() string      { return s.appName }
func (s *fileSession) UserID() string       { return s.userID }
func (s *fileSession) State() session.State { return s.state }
func (s *fileSession) LastUpdateTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updatedAt
}
func (s *fileSession) Events() session.Events {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fileSessionEvents(slices.Clone(s.events))
}

type fileSessionState struct {
	mu           sync.RWMutex
	fs           *fileSession
	sessionState map[string]any
	merged       map[string]any
}

func (st *fileSessionState) recomputeMerged(svc *FileSessionService, appName, userID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.merged = svc.mergeStatesLocked(st.sessionState, appName, userID)
}

func (st *fileSessionState) Get(key string) (any, error) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	v, ok := st.merged[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}

func (st *fileSessionState) Set(key string, value any) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.merged == nil {
		st.merged = make(map[string]any)
	}
	st.merged[key] = value

	if strings.HasPrefix(key, session.KeyPrefixApp) {
		if st.fs != nil && st.fs.svc != nil {
			st.fs.svc.mu.Lock()
			st.fs.svc.updateAppStateLocked(map[string]any{key: value}, st.fs.appName)
			st.fs.svc.mu.Unlock()
		}
	} else if strings.HasPrefix(key, session.KeyPrefixUser) {
		if st.fs != nil && st.fs.svc != nil {
			st.fs.svc.mu.Lock()
			st.fs.svc.updateUserStateLocked(map[string]any{key: value}, st.fs.appName, st.fs.userID)
			st.fs.svc.mu.Unlock()
		}
	} else if !strings.HasPrefix(key, session.KeyPrefixTemp) {
		if st.sessionState == nil {
			st.sessionState = make(map[string]any)
		}
		st.sessionState[key] = value
	}
	return nil
}

func (st *fileSessionState) All() iter.Seq2[string, any] {
	st.mu.RLock()
	cp := maps.Clone(st.merged)
	st.mu.RUnlock()
	return func(yield func(string, any) bool) {
		for k, v := range cp {
			if !yield(k, v) {
				return
			}
		}
	}
}

type fileSessionEvents []*session.Event

func (e fileSessionEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, event := range e {
			if !yield(event) {
				return
			}
		}
	}
}

func (e fileSessionEvents) Len() int {
	return len(e)
}

func (e fileSessionEvents) At(i int) *session.Event {
	if i >= 0 && i < len(e) {
		return e[i]
	}
	return nil
}

func cloneEvent(event *session.Event) *session.Event {
	if event == nil {
		return nil
	}
	cp := *event
	cp.Actions = session.EventActions{
		StateDelta:                 maps.Clone(event.Actions.StateDelta),
		ArtifactDelta:              maps.Clone(event.Actions.ArtifactDelta),
		RequestedToolConfirmations: maps.Clone(event.Actions.RequestedToolConfirmations),
		TransferToAgent:            event.Actions.TransferToAgent,
		Escalate:                   event.Actions.Escalate,
		SkipSummarization:          event.Actions.SkipSummarization,
	}
	cp.LongRunningToolIDs = slices.Clone(event.LongRunningToolIDs)
	cp.Routes = slices.Clone(event.Routes)
	return &cp
}

func trimTempDeltaState(event *session.Event) *session.Event {
	if len(event.Actions.StateDelta) == 0 {
		return event
	}
	filtered := make(map[string]any)
	for k, v := range event.Actions.StateDelta {
		if !strings.HasPrefix(k, session.KeyPrefixTemp) {
			filtered[k] = v
		}
	}
	event.Actions.StateDelta = filtered
	return event
}

func extractStateDeltas(state map[string]any) (appDelta, userDelta, sessionDelta map[string]any) {
	appDelta = make(map[string]any)
	userDelta = make(map[string]any)
	sessionDelta = make(map[string]any)
	for k, v := range state {
		if strings.HasPrefix(k, session.KeyPrefixApp) {
			appDelta[k] = v
		} else if strings.HasPrefix(k, session.KeyPrefixUser) {
			userDelta[k] = v
		} else if !strings.HasPrefix(k, session.KeyPrefixTemp) {
			sessionDelta[k] = v
		}
	}
	return appDelta, userDelta, sessionDelta
}

func saveStoredSessionFileAtomic(path string, file *StoredSessionFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
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

// ReadStoredSessionFile reads and parses a StoredSessionFile from disk.
func ReadStoredSessionFile(path string) (StoredSessionFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return StoredSessionFile{}, err
	}
	var file StoredSessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return StoredSessionFile{}, err
	}
	if file.Version != 1 {
		return StoredSessionFile{}, fmt.Errorf("unsupported session version %d", file.Version)
	}
	return file, nil
}

// SessionPreviewFromEvents extracts the first user prompt line to serve as a preview.
func SessionPreviewFromEvents(events []*session.Event) string {
	for _, e := range events {
		if e == nil {
			continue
		}
		if e.Author == "user" || (e.LLMResponse.Content != nil && e.LLMResponse.Content.Role == genai.RoleUser) {
			if e.LLMResponse.Content != nil {
				for _, p := range e.LLMResponse.Content.Parts {
					if p != nil && p.Text != "" {
						line := strings.TrimSpace(strings.SplitN(p.Text, "\n", 2)[0])
						if len(line) > 80 {
							line = line[:80] + "…"
						}
						if line != "" {
							return line
						}
					}
				}
			}
		}
	}
	return "(empty session)"
}

// SessionPreview extracts the first user prompt line from messages.
func SessionPreview(messages []Message) string {
	for _, m := range messages {
		if m.Role != RoleUser {
			continue
		}
		if m.Text != "" {
			line := strings.TrimSpace(strings.SplitN(m.Text, "\n", 2)[0])
			if len(line) > 80 {
				line = line[:80] + "…"
			}
			if line != "" {
				return line
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

// SessionStore provides backwards compatibility adapter for existing callers.
type SessionStore struct {
	svc *FileSessionService
}

// NewSessionStore creates a new SessionStore adapter wrapping FileSessionService.
func NewSessionStore(provider string) *SessionStore {
	return &SessionStore{svc: NewFileSessionService(provider, "")}
}

func (st *SessionStore) Root() (string, error) {
	return st.svc.Root()
}

func (st *SessionStore) DirFor(cwd string) (string, error) {
	return st.svc.DirFor(cwd)
}

func (st *SessionStore) PathFor(id string) (string, error) {
	return st.svc.PathFor(id)
}

func (st *SessionStore) Save(id, cwd string, messages []Message) error {
	var events []*session.Event
	for _, m := range messages {
		author := "user"
		if m.Role == RoleAssistant || m.Role == RoleModel {
			author = "ask_coder"
		}
		events = append(events, &session.Event{
			Author: author,
			LLMResponse: adkmodel.LLMResponse{
				Content: m.ToGenAIContent(),
			},
			Timestamp: time.Now(),
		})
	}
	return st.SaveEvents(id, cwd, events)
}

func (st *SessionStore) SaveEvents(id, cwd string, events []*session.Event) error {
	dir, err := st.svc.DirFor(cwd)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, id+".json")
	now := time.Now()
	file := StoredSessionFile{
		Version:   1,
		AppName:   "ask",
		UserID:    "user",
		SessionID: id,
		Cwd:       cwd,
		CreatedAt: now,
		UpdatedAt: now,
		Events:    events,
	}
	if prev, err := ReadStoredSessionFile(path); err == nil && !prev.CreatedAt.IsZero() {
		file.CreatedAt = prev.CreatedAt
		file.State = prev.State
	}
	return saveStoredSessionFileAtomic(path, &file)
}

func (st *SessionStore) Load(id string) (StoredSessionFile, error) {
	path, err := st.svc.PathFor(id)
	if err != nil {
		return StoredSessionFile{}, err
	}
	return ReadStoredSessionFile(path)
}

func (st *SessionStore) Delete(id string) error {
	path, err := st.svc.PathFor(id)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (st *SessionStore) List(cwd string) ([]SessionSummary, error) {
	dir, err := st.svc.DirFor(cwd)
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
		file, err := ReadStoredSessionFile(path)
		if err != nil {
			continue
		}
		sessions = append(sessions, SessionSummary{
			ID:      strings.TrimSuffix(e.Name(), ".json"),
			Cwd:     file.Cwd,
			Preview: SessionPreviewFromEvents(file.Events),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})
	return sessions, nil
}
