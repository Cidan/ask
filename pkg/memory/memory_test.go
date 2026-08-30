package memory

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestService(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)}
	svc, err := NewService(Options{
		DBPath:   filepath.Join(t.TempDir(), "memory.db"),
		Embedder: NewFakeEmbedder(512),
		Now:      clock.Now,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc, clock
}

func TestUpsertAndGet(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "proj")

	id, err := svc.Upsert(ctx, Concept{Scope: ScopeFor(cwd), Kind: KindFeedback, Topic: " Code  Style ", Title: "Keep answers short", Body: "Bullets over prose."})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != KindFeedback || got.Title != "Keep answers short" || got.Body != "Bullets over prose." || got.Topic != "code style" || got.Weight != WeightInitial {
		t.Fatalf("unexpected concept: %+v", got)
	}

	// Rewriting by id keeps the id and re-embeds under the new text.
	id2, err := svc.Upsert(ctx, Concept{ID: id, Scope: got.Scope, Kind: KindFeedback, Title: "Keep answers terse", Body: "One idea per line."})
	if err != nil || id2 != id {
		t.Fatalf("update: id=%d err=%v", id2, err)
	}
	res, err := svc.Recall(ctx, RecallQuery{Cwd: cwd, Query: "terse answers one idea per line", K: 5, Silent: true})
	if err != nil || len(res.Concepts) != 1 || res.Concepts[0].Title != "Keep answers terse" {
		t.Fatalf("recall after update: %+v err=%v", res, err)
	}

	// Invalid kind defaults, missing title derives from the body.
	id3, err := svc.Upsert(ctx, Concept{Scope: ScopeGlobal, Kind: "bogus", Body: "First line is the title\nrest is body"})
	if err != nil {
		t.Fatalf("Upsert derived: %v", err)
	}
	c3, _ := svc.Get(ctx, id3)
	if c3.Kind != KindProject || c3.Title != "First line is the title" {
		t.Fatalf("derived concept: %+v", c3)
	}
	if _, err := svc.Upsert(ctx, Concept{Scope: ScopeGlobal}); err == nil {
		t.Fatal("empty concept must error")
	}
	if _, err := svc.Upsert(ctx, Concept{Title: "no scope"}); err == nil {
		t.Fatal("missing scope must error")
	}
	// Unknown id inserts rather than failing.
	id4, err := svc.Upsert(ctx, Concept{ID: 9999, Scope: ScopeGlobal, Title: "fresh"})
	if err != nil || id4 == 9999 || id4 == 0 {
		t.Fatalf("unknown id upsert: id=%d err=%v", id4, err)
	}
}

func TestRecallScopesProjectAndGlobal(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	tmp := t.TempDir()
	projA, projB := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")

	mustUpsert := func(c Concept) int64 {
		t.Helper()
		id, err := svc.Upsert(ctx, c)
		if err != nil {
			t.Fatalf("Upsert %q: %v", c.Title, err)
		}
		return id
	}
	mustUpsert(Concept{Scope: ScopeFor(projA), Kind: KindProject, Title: "deploy pipeline requires staging validation"})
	mustUpsert(Concept{Scope: ScopeFor(projB), Kind: KindProject, Title: "deploy pipeline requires staging validation"})
	mustUpsert(Concept{Scope: ScopeGlobal, Kind: KindUser, Title: "deploy pipeline requires staging validation always"})

	res, err := svc.Recall(ctx, RecallQuery{Cwd: projA, Query: "deploy pipeline requires staging validation", K: 10})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	var scopes []string
	for _, c := range res.Concepts {
		scopes = append(scopes, c.Scope)
	}
	joined := strings.Join(scopes, "|")
	if len(scopes) != 2 || !strings.Contains(joined, ScopeFor(projA)) || !strings.Contains(joined, ScopeGlobal) {
		t.Fatalf("project A recall must be its own plus global concepts: %v", scopes)
	}
	if strings.Contains(joined, ScopeFor(projB)) {
		t.Fatalf("project B concept leaked into project A recall: %v", scopes)
	}

	// Empty cwd searches every scope.
	all, err := svc.Recall(ctx, RecallQuery{Query: "deploy pipeline requires staging validation", K: 10})
	if err != nil || len(all.Concepts) != 3 {
		t.Fatalf("unscoped recall = %d concepts, err=%v", len(all.Concepts), err)
	}
	// Unrelated queries find nothing.
	none, err := svc.Recall(ctx, RecallQuery{Cwd: projA, Query: "kitten photography", K: 10})
	if err != nil || len(none.Concepts) != 0 {
		t.Fatalf("unrelated recall = %+v err=%v", none.Concepts, err)
	}
}

func TestRecallReranksByWeightAndBumps(t *testing.T) {
	svc, clock := newTestService(t)
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "proj")
	scope := ScopeFor(cwd)

	cold, _ := svc.Upsert(ctx, Concept{Scope: scope, Kind: KindProject, Title: "release checklist smoke tests run first"})
	hot, _ := svc.Upsert(ctx, Concept{Scope: scope, Kind: KindProject, Title: "release checklist smoke tests tag build"})
	for i := 0; i < 3; i++ {
		clock.Advance(2 * time.Minute)
		if _, err := svc.Reinforce(ctx, hot); err != nil {
			t.Fatalf("Reinforce: %v", err)
		}
	}
	res, err := svc.Recall(ctx, RecallQuery{Cwd: cwd, Query: "release checklist smoke tests", K: 2})
	if err != nil || len(res.Concepts) != 2 {
		t.Fatalf("Recall: %+v err=%v", res, err)
	}
	if res.Concepts[0].ID != hot {
		t.Fatalf("reinforced concept must rank first: got #%d then #%d", res.Concepts[0].ID, res.Concepts[1].ID)
	}
	if res.Concepts[0].Score <= res.Concepts[1].Score {
		t.Fatalf("scores must be ordered: %v", []float64{res.Concepts[0].Score, res.Concepts[1].Score})
	}

	// The recall bumped both.
	c, _ := svc.Get(ctx, cold)
	if c.AccessCount != 1 || c.Weight <= WeightInitial {
		t.Fatalf("implicit bump missing: %+v", c)
	}
	before := c.Weight
	_, _ = svc.Recall(ctx, RecallQuery{Cwd: cwd, Query: "release checklist", K: 2, Silent: true})
	c, _ = svc.Get(ctx, cold)
	if c.AccessCount != 1 || c.Weight != before {
		t.Fatalf("silent recall must not bump: %+v", c)
	}
}

func TestDecayReinforceDemoteRefractory(t *testing.T) {
	svc, clock := newTestService(t)
	ctx := context.Background()
	id, _ := svc.Upsert(ctx, Concept{Scope: ScopeGlobal, Kind: KindUser, Title: "user is a Go developer"})

	clock.Advance(ConceptHalfLife)
	c, _ := svc.Get(ctx, id)
	if math.Abs(c.Weight-WeightInitial/2) > 0.01 {
		t.Fatalf("after one half-life weight = %v, want ~%v", c.Weight, WeightInitial/2)
	}

	applied, err := svc.Reinforce(ctx, id)
	if err != nil || !applied {
		t.Fatalf("Reinforce applied=%v err=%v", applied, err)
	}
	c, _ = svc.Get(ctx, id)
	want := 0.5 + ExplicitBump*(1-0.5/WeightCap)
	if math.Abs(c.Weight-want) > 0.01 {
		t.Fatalf("reinforced weight = %v want ~%v (log-dampened)", c.Weight, want)
	}

	// A second explicit bump inside the refractory window is dropped.
	clock.Advance(10 * time.Second)
	applied, err = svc.Reinforce(ctx, id)
	if err != nil || applied {
		t.Fatalf("refractory: applied=%v err=%v", applied, err)
	}
	after, _ := svc.Get(ctx, id)
	if math.Abs(after.Weight-c.Weight) > 0.001 {
		t.Fatalf("refractory bump changed weight: %v -> %v", c.Weight, after.Weight)
	}

	clock.Advance(RefractoryPeriod)
	for i := 0; i < 20; i++ {
		clock.Advance(RefractoryPeriod)
		_, _ = svc.Reinforce(ctx, id)
	}
	c, _ = svc.Get(ctx, id)
	if c.Weight > WeightCap || c.Weight < WeightCap-0.5 {
		t.Fatalf("repeated reinforce must approach but not exceed the cap: %v", c.Weight)
	}

	clock.Advance(RefractoryPeriod)
	for i := 0; i < 20; i++ {
		clock.Advance(RefractoryPeriod)
		_, _ = svc.Demote(ctx, id)
	}
	c, _ = svc.Get(ctx, id)
	if c.Weight != WeightFloor {
		t.Fatalf("demote must clamp at the floor, got %v", c.Weight)
	}
	if _, err := svc.Get(ctx, id); err != nil {
		t.Fatal("demoted concept must still exist")
	}

	if _, err := svc.Reinforce(ctx, 424242); err == nil {
		t.Fatal("reinforcing an unknown id must error")
	}
}

func TestForget(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	id, _ := svc.Upsert(ctx, Concept{Scope: ScopeGlobal, Kind: KindReference, Title: "dashboard lives at grafana"})
	if err := svc.Forget(ctx, id); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := svc.Get(ctx, id); err == nil {
		t.Fatal("forgotten concept must be gone")
	}
	res, _ := svc.Recall(ctx, RecallQuery{Query: "dashboard grafana", K: 5})
	if len(res.Concepts) != 0 {
		t.Fatalf("forgotten concept still recalled: %+v", res.Concepts)
	}
	if err := svc.Forget(ctx, id); err == nil {
		t.Fatal("forgetting twice must error")
	}
}

func TestTopAndTopics(t *testing.T) {
	svc, clock := newTestService(t)
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "proj")
	scope := ScopeFor(cwd)

	a, _ := svc.Upsert(ctx, Concept{Scope: scope, Kind: KindProject, Topic: "memory", Title: "alpha"})
	b, _ := svc.Upsert(ctx, Concept{Scope: scope, Kind: KindProject, Topic: "memory", Title: "beta"})
	g, _ := svc.Upsert(ctx, Concept{Scope: ScopeGlobal, Kind: KindUser, Topic: "style", Title: "gamma"})
	_, _ = svc.Upsert(ctx, Concept{Scope: "/elsewhere", Kind: KindProject, Topic: "other", Title: "delta"})
	clock.Advance(time.Minute)
	_, _ = svc.Reinforce(ctx, b)

	top, err := svc.Top(ctx, cwd, 10)
	if err != nil {
		t.Fatalf("Top: %v", err)
	}
	if len(top) != 3 || top[0].ID != b {
		var ids []int64
		for _, c := range top {
			ids = append(ids, c.ID)
		}
		t.Fatalf("Top = %v, want [%d %d|%d %d|%d] with %d first", ids, b, a, g, a, g, b)
	}
	if top2, _ := svc.Top(ctx, cwd, 2); len(top2) != 2 {
		t.Fatalf("Top k=2 returned %d", len(top2))
	}

	names := svc.TopicNames(ctx, cwd, 10)
	if strings.Join(names, ",") != "memory,style" {
		t.Fatalf("topics = %v, want [memory style]", names)
	}
	if err := svc.TouchTopic(ctx, cwd, "Memory"); err != nil {
		t.Fatalf("TouchTopic: %v", err)
	}
	topics, _ := svc.Topics(ctx, cwd, 10)
	if topics[0].Name != "memory" || topics[0].Weight <= WeightInitial {
		t.Fatalf("touched topic must lead and be bumped: %+v", topics)
	}
	if err := svc.TouchTopic(ctx, "", "cross project"); err != nil {
		t.Fatalf("TouchTopic global: %v", err)
	}
	names = svc.TopicNames(ctx, cwd, 10)
	if len(names) != 3 || !strings.Contains(strings.Join(names, ","), "cross project") {
		t.Fatalf("global topic must be visible from a project: %v", names)
	}
	elsewhere := strings.Join(svc.TopicNames(ctx, "/elsewhere", 10), ",")
	if strings.Contains(elsewhere, "memory") || !strings.Contains(elsewhere, "other") || !strings.Contains(elsewhere, "style") || !strings.Contains(elsewhere, "cross project") {
		t.Fatalf("other project sees its own topics plus global, never another project's: %v", elsewhere)
	}
}

func TestRecallInfersTopicAndBoosts(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "proj")
	scope := ScopeFor(cwd)

	_, _ = svc.Upsert(ctx, Concept{Scope: scope, Kind: KindFeedback, Topic: "communication", Title: "answers should be short"})
	_, _ = svc.Upsert(ctx, Concept{Scope: scope, Kind: KindFeedback, Topic: "communication", Title: "answers should be brief"})
	other, _ := svc.Upsert(ctx, Concept{Scope: scope, Kind: KindProject, Topic: "deploy", Title: "answers should be about deploys"})

	res, err := svc.Recall(ctx, RecallQuery{Cwd: cwd, Query: "answers should be short", Topic: "deploy", K: 10})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if res.Topic != "communication" {
		t.Fatalf("inferred topic = %q, want communication (two hits agree)", res.Topic)
	}
	if len(res.Concepts) != 3 || res.Concepts[len(res.Concepts)-1].ID != other {
		t.Fatalf("off-topic concept must rank last after the topic boost: %+v", res.Concepts)
	}

	// No agreement among hits: fall back to the caller's topic.
	single, _ := svc.Recall(ctx, RecallQuery{Cwd: cwd, Query: "staging deploys", Topic: "Deploy Ops", K: 10})
	if single.Topic != "deploy ops" {
		t.Fatalf("fallback topic = %q", single.Topic)
	}
	empty, _ := svc.Recall(ctx, RecallQuery{Cwd: cwd, Query: "   ", Topic: "x"})
	if empty.Topic != "x" || len(empty.Concepts) != 0 {
		t.Fatalf("empty query: %+v", empty)
	}
}

func TestLegacyMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
		CREATE TABLE project_memory (id INTEGER PRIMARY KEY AUTOINCREMENT, project_id TEXT, text_payload TEXT, last_recalled_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO project_memory (project_id, text_payload) VALUES ('/home/u/proj', 'FEEDBACK: never mock the database.' || char(10) || 'WHY: prod migration failed.');
		INSERT INTO project_memory (project_id, text_payload) VALUES ('', 'user prefers dark mode');
		INSERT INTO project_memory (project_id, text_payload) VALUES ('/home/u/proj', '   ');
	`)
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()

	svc, err := NewService(Options{DBPath: dbPath, Embedder: NewFakeEmbedder(512)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()
	ctx := context.Background()
	top, err := svc.Top(ctx, "", 10)
	if err != nil || len(top) != 2 {
		t.Fatalf("migrated concepts = %d err=%v: %+v", len(top), err, top)
	}
	byTitle := map[string]Concept{}
	for _, c := range top {
		byTitle[c.Title] = c
	}
	fb, ok := byTitle["FEEDBACK: never mock the database."]
	if !ok || fb.Scope != "/home/u/proj" || !strings.Contains(fb.Body, "WHY") || fb.Kind != KindProject {
		t.Fatalf("legacy row not converted: %+v", byTitle)
	}
	if dm := byTitle["user prefers dark mode"]; dm.Scope != ScopeGlobal {
		t.Fatalf("empty project id must become global: %+v", dm)
	}
	var n int
	if err := svc.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('project_memory','vec_memory')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("legacy tables must be dropped: n=%d err=%v", n, err)
	}
	res, _ := svc.Recall(ctx, RecallQuery{Cwd: "/home/u/proj", Query: "mock the database", K: 5})
	if len(res.Concepts) != 1 {
		t.Fatalf("migrated concept not recallable: %+v", res.Concepts)
	}
	// Reopening is a no-op.
	svc.Close()
	again, err := NewService(Options{DBPath: dbPath, Embedder: NewFakeEmbedder(512)})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if top, _ := again.Top(ctx, "", 10); len(top) != 2 {
		t.Fatalf("reopen duplicated concepts: %d", len(top))
	}
}

func TestFormatConcepts(t *testing.T) {
	concepts := []Concept{
		{ID: 1, Kind: KindFeedback, Topic: "style", Title: "Short answers", Body: "Bullets.\nNo prose."},
		{ID: 2, Kind: KindUser, Scope: ScopeGlobal, Title: "Go developer", Body: "Go developer"},
		{ID: 3, Kind: KindProject, Title: "Third", Body: "hidden body"},
	}
	got := FormatConcepts(concepts, "Project memory", "note", 2)
	want := "## Project memory\nnote\n\n- #1 [feedback · style] Short answers\n  Bullets.\n  No prose.\n- #2 [user · global] Go developer\n- #3 [project] Third"
	if got != want {
		t.Fatalf("FormatConcepts:\n%s\nwant:\n%s", got, want)
	}
	if FormatConcepts(nil, "x", "", 1) != "" {
		t.Fatal("empty list must render nothing")
	}
}

func TestPromptBlocksThroughDefault(t *testing.T) {
	_ = Close()
	defer Close()
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "proj")
	if SystemBlock(ctx, cwd) != "" || TopIDs(ctx, cwd) != nil || FileBlock(ctx, cwd, "main.go") != "" {
		t.Fatal("closed service must render nothing")
	}
	if block, topic := RecallBlock(ctx, cwd, "anything", "T", nil); block != "" || topic != "t" {
		t.Fatalf("closed RecallBlock = %q/%q", block, topic)
	}
	if err := Open(Options{DBPath: filepath.Join(t.TempDir(), "d.db"), Embedder: NewFakeEmbedder(512)}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	top, _ := Upsert(ctx, Concept{Scope: ScopeFor(cwd), Kind: KindProject, Topic: "build", Title: "make test builds llama"})
	other, _ := Upsert(ctx, Concept{Scope: ScopeFor(cwd), Kind: KindProject, Topic: "build", Title: "make test downloads model"})

	sys := SystemBlock(ctx, cwd)
	if !strings.Contains(sys, "## Project memory") || !strings.Contains(sys, "builds llama") {
		t.Fatalf("SystemBlock: %q", sys)
	}
	ids := TopIDs(ctx, cwd)
	if !ids[top] || !ids[other] {
		t.Fatalf("TopIDs = %v", ids)
	}
	block, topic := RecallBlock(ctx, cwd, "make test", "", ids)
	if block != "" {
		t.Fatalf("ids in the system block must be excluded: %q", block)
	}
	if topic != "build" {
		t.Fatalf("topic = %q", topic)
	}
	block, _ = RecallBlock(ctx, cwd, "make test", "", nil)
	if !strings.Contains(block, "## Relevant memory") || !strings.Contains(block, "builds llama") {
		t.Fatalf("RecallBlock: %q", block)
	}
	file := FileBlock(ctx, cwd, "make test")
	if !strings.Contains(file, "## Memory for make test") {
		t.Fatalf("FileBlock: %q", file)
	}
}

type fakeExtractor struct {
	mu   sync.Mutex
	recs []TurnRecord
}

func (f *fakeExtractor) Enqueue(r TurnRecord) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
	return true
}

func TestAddSessionToMemoryHandsLastExchangeToExtractor(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	sessSvc := session.InMemoryService()
	created, err := sessSvc.Create(ctx, &session.CreateRequest{AppName: "ask", UserID: "user", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	append := func(role genai.Role, text string) {
		ev := session.NewEvent(ctx, "inv")
		ev.Content = genai.NewContentFromText(text, role)
		_ = sessSvc.AppendEvent(ctx, created.Session, ev)
	}
	append(genai.RoleUser, "first question")
	append(genai.RoleModel, "first answer")
	append(genai.RoleUser, "second question")
	append(genai.RoleModel, "part one")
	append(genai.RoleModel, "part two")

	if err := svc.AddSessionToMemory(ctx, created.Session); err == nil {
		t.Fatal("without an extractor AddSessionToMemory must error")
	}
	fx := &fakeExtractor{}
	svc.SetExtractor(fx)
	if err := svc.AddSessionToMemory(ctx, created.Session); err != nil {
		t.Fatalf("AddSessionToMemory: %v", err)
	}
	if len(fx.recs) != 1 || fx.recs[0].Prompt != "second question" || fx.recs[0].Response != "part one\npart two" {
		t.Fatalf("extractor got %+v", fx.recs)
	}
	var closed *Service
	if err := closed.AddSessionToMemory(ctx, nil); err == nil {
		t.Fatal("closed service must error")
	}
}

func TestSearchMemoryADKShape(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	_, _ = svc.Upsert(ctx, Concept{Scope: "/p", Kind: KindProject, Topic: "deploy", Title: "staging validation first", Body: "Deploy pipeline requires staging validation first."})

	resp, err := svc.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "staging validation pipeline"})
	if err != nil || len(resp.Memories) != 1 {
		t.Fatalf("SearchMemory: %+v err=%v", resp, err)
	}
	entry := resp.Memories[0]
	if entry.ID == "" || entry.CustomMetadata["kind"] != KindProject || entry.CustomMetadata["topic"] != "deploy" {
		t.Fatalf("entry: %+v", entry)
	}
	if !strings.Contains(entry.Content.Parts[0].Text, "staging validation first") {
		t.Fatalf("content: %q", entry.Content.Parts[0].Text)
	}
	if empty, _ := svc.SearchMemory(ctx, &adkmemory.SearchRequest{Query: " "}); len(empty.Memories) != 0 {
		t.Fatal("blank query must return nothing")
	}
	var closed *Service
	if r, err := closed.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "x"}); err != nil || len(r.Memories) != 0 {
		t.Fatal("closed service must return an empty response")
	}
}

func TestConcurrentAccess(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	cwd := filepath.Join(t.TempDir(), "proj")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = svc.Upsert(ctx, Concept{Scope: ScopeFor(cwd), Kind: KindProject, Title: "concurrent entry"})
		}()
		go func() {
			defer wg.Done()
			_, _ = svc.Recall(ctx, RecallQuery{Cwd: cwd, Query: "concurrent entry", K: 3})
		}()
	}
	wg.Wait()
}

func TestWeightMath(t *testing.T) {
	if w := decayedWeight(2, 0, ConceptHalfLife); w != 2 {
		t.Fatalf("no time, no decay: %v", w)
	}
	if w := decayedWeight(WeightInitial, 1000*ConceptHalfLife, ConceptHalfLife); w != WeightFloor {
		t.Fatalf("decay floors at %v, got %v", WeightFloor, w)
	}
	if w := bumpWeight(WeightCap, ExplicitBump, true); w != WeightCap {
		t.Fatalf("dampened bump at the cap must stay at the cap: %v", w)
	}
	if w := bumpWeight(1, -10, true); w != WeightFloor {
		t.Fatalf("negative bumps clamp at the floor: %v", w)
	}
	if rerankScore(0, 4) <= rerankScore(0, 1) || rerankScore(0.3, 1) >= rerankScore(0, 1) {
		t.Fatal("score must grow with weight and shrink with distance")
	}
	if NormalizeTopic("  Memory   Design  Notes extra ") != "memory design notes" {
		t.Fatal("NormalizeTopic must lowercase, collapse, and cap at three words")
	}
	if TitleFromBody("\n\n"+strings.Repeat("x", 100)) == "" || len([]rune(TitleFromBody(strings.Repeat("x", 100)))) > 80 {
		t.Fatal("TitleFromBody must clip long first lines")
	}
}

func TestNewService_RequiresEmbedder(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	if _, err := NewService(Options{DBPath: dbPath}); err == nil {
		t.Fatal("NewService without an Embedder must fail instead of loading a model itself")
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("a rejected NewService must not create the database, stat err=%v", err)
	}
}
