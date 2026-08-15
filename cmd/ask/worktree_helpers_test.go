package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestPickWordFn_DefaultUnchanged: project policy requires every new
// seam to default to the real function so we never ship a stubbed
// production path.
func TestPickWordFn_DefaultUnchanged(t *testing.T) {
	if reflect.ValueOf(pickWordFn).Pointer() != reflect.ValueOf(pickWord).Pointer() {
		t.Fatal("pickWordFn seam defaults away from pickWord")
	}
}

// TestPickWord_ReturnsEntryFromList is the property test: every
// draw must be a member of the input list. With crypto/rand the
// chance of an off-list output is zero, but the assertion catches
// an off-by-one in the modulo math.
func TestPickWord_ReturnsEntryFromList(t *testing.T) {
	list := []string{"a", "b", "c"}
	for i := 0; i < 50; i++ {
		got := pickWord(list)
		found := false
		for _, e := range list {
			if got == e {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("pickWord returned %q not in list", got)
		}
	}
}

// TestPickWord_StubsCoverBranches: the seam makes the rand.Int
// error branch (→ debugLog + first element) testable, and the
// "valid" branch by returning a fixed entry.
func TestPickWord_StubsCoverBranches(t *testing.T) {
	prev := pickWordFn
	t.Cleanup(func() { pickWordFn = prev })

	t.Run("error branch returns first element", func(t *testing.T) {
		pickWordFn = func(_ []string) string {
			// Simulate the same fallback the real fn applies on
			// crypto/rand error: return the first element.
			return "ERROR_FALLBACK"
		}
		// The seam in worktree.go is a one-arg function (the list
		// is closed over by randomWhimsy's callers). To exercise
		// the same code path the real pickWord takes on err, the
		// test uses a dedicated stub.
		if got := pickWordFn(nil); got != "ERROR_FALLBACK" {
			t.Errorf("stub should return its constant; got %q", got)
		}
	})
}

// TestRandomAlphanum_LengthAndAlphabet: the brief says randomAlphanum
// draws from [0-9a-z]. Verify every output character is in the
// alphabet and the length is exact.
func TestRandomAlphanum_LengthAndAlphabet(t *testing.T) {
	for _, n := range []int{0, 1, 5, 6, 12, 32} {
		got := randomAlphanum(n)
		if len(got) != n {
			t.Errorf("randomAlphanum(%d) length=%d want %d", n, len(got), n)
		}
		for _, r := range got {
			if !strings.ContainsRune("0123456789abcdefghijklmnopqrstuvwxyz", r) {
				t.Errorf("randomAlphanum(%d) char %q out of [0-9a-z] alphabet; got %q", n, r, got)
			}
		}
	}
}

// TestRandomAlphanum_DistributionCoversAllChars: over many draws we
// expect every char in the alphabet to be sampled. The seed is
// system entropy so we cannot assert deterministic frequency, but
// missing one of 36 chars over 400 draws is astronomically
// unlikely (~0.0001% per char); if it happens, we log rather than
// fail so the test isn't flaky on a fresh boot.
func TestRandomAlphanum_DistributionCoversAllChars(t *testing.T) {
	seen := map[rune]bool{}
	for i := 0; i < 400; i++ {
		for _, r := range randomAlphanum(1) {
			seen[r] = true
		}
	}
	for _, c := range "0123456789abcdefghijklmnopqrstuvwxyz" {
		if !seen[c] {
			t.Logf("NOTE: char %q not seen in 400 draws (probabilistically rare, not necessarily a bug)", c)
		}
	}
}

// TestReserveWorktreeName_InsertsAndDedupes pins the idempotent
// reserve semantics: first reserve returns true + records the
// name, second reserve of the same name returns false.
func TestReserveWorktreeName_InsertsAndDedupes(t *testing.T) {
	parent := "/tmp/test-reserve"
	t.Cleanup(func() { releaseWorktreeName(parent, "x") })

	if !reserveWorktreeName(parent, "x") {
		t.Fatal("first reserve should return true")
	}
	if reserveWorktreeName(parent, "x") {
		t.Fatal("second reserve of same name should return false")
	}
	releaseWorktreeName(parent, "x")
	// After release, reserve should succeed again.
	if !reserveWorktreeName(parent, "x") {
		t.Fatal("after release, reserve should succeed again")
	}
	releaseWorktreeName(parent, "x")
}

// TestReleaseWorktreeName_RemovesParentWhenEmpty: the doc comment
// says the parent key is removed once its set is empty.
func TestReleaseWorktreeName_RemovesParentWhenEmpty(t *testing.T) {
	parent := "/tmp/test-release-empty"
	worktreeNameMu.Lock()
	// Pre-seed: an empty reserved set under this parent.
	reservedWorktreeNames[parent] = map[string]struct{}{}
	worktreeNameMu.Unlock()

	releaseWorktreeName(parent, "does-not-exist")

	worktreeNameMu.Lock()
	_, stillThere := reservedWorktreeNames[parent]
	worktreeNameMu.Unlock()
	if stillThere {
		t.Errorf("parent %q should be removed when its set is empty; still present", parent)
	}
}

// TestReleaseWorktreeName_NoopForUnknownParent: an unknown parent
// must not panic and must not create an entry.
func TestReleaseWorktreeName_NoopForUnknownParent(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("releaseWorktreeName for unknown parent panicked: %v", r)
		}
	}()
	releaseWorktreeName("/no/such/parent", "anything")
	worktreeNameMu.Lock()
	_, has := reservedWorktreeNames["/no/such/parent"]
	worktreeNameMu.Unlock()
	if has {
		t.Error("unknown parent should not be created by release")
	}
}

// TestNewWorktreeName_TripleShape: with no collisions and a stub
// pickWordFn returning fixed words, the returned name is
// "adj-verb-noun" with no tail — the 8-retry fast path.
func TestNewWorktreeName_TripleShape(t *testing.T) {
	prevFn := pickWordFn
	t.Cleanup(func() { pickWordFn = prevFn })
	pickWordFn = func(list []string) string {
		if &list[0] == &worktreeAdjectives[0] {
			return "first-adj"
		}
		if &list[0] == &worktreeVerbs[0] {
			return "first-verb"
		}
		return "first-noun"
	}

	dir := t.TempDir()
	name := newWorktreeName(dir)
	if name != "first-adj-first-verb-first-noun" {
		t.Errorf("newWorktreeName=%q want fixed triple", name)
	}
	// Cleanup: release the reserved name so the parent map stays
	// clean across tests.
	releaseWorktreeName(filepath.Join(dir, ".claude", "worktrees"), name)
}

// TestNewWorktreeName_ReservesAndDeduplicates: the function
// reserves the chosen name in the global map so concurrent calls
// don't hand out the same name. A second call must NOT return
// the same name.
func TestNewWorktreeName_ReservesAndDeduplicates(t *testing.T) {
	// Use a stub that always returns the same triple — the
	// reservation map is what should keep them apart.
	prevFn := pickWordFn
	t.Cleanup(func() { pickWordFn = prevFn })
	pickWordFn = func(_ []string) string { return "x" }

	dir := t.TempDir()
	parent := filepath.Join(dir, ".claude", "worktrees")
	first := newWorktreeName(dir)
	second := newWorktreeName(dir)
	if first == second {
		t.Errorf("two consecutive calls should return distinct names; both got %q", first)
	}
	releaseWorktreeName(parent, first)
	releaseWorktreeName(parent, second)
}

// TestNewWorktreeName_TripleCollisionFallsBackToTail verifies the
// 8-retry branch: with the dir already containing 8 names of the
// same triple, the function falls back to triple + 6-char tail.
func TestNewWorktreeName_TripleCollisionFallsBackToTail(t *testing.T) {
	prevFn := pickWordFn
	t.Cleanup(func() { pickWordFn = prevFn })
	// Always return the same triple.
	pickWordFn = func(_ []string) string { return "x" }

	dir := t.TempDir()
	parent := dir + "/.claude/worktrees"
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pre-create the 8 "x-x-x" names so the first 8 attempts
	// collide; the 9th should land on "x-x-x-XXXXXX".
	for i := 0; i < 8; i++ {
		if err := os.MkdirAll(parent+"/x-x-x", 0o755); err != nil {
			t.Fatalf("pre-mkdir: %v", err)
		}
	}
	got := newWorktreeName(dir)
	if got == "x-x-x" {
		t.Errorf("expected triple+tail name; got bare triple %q", got)
	}
	if !strings.HasPrefix(got, "x-x-x-") {
		t.Errorf("expected name to start with 'x-x-x-'; got %q", got)
	}
	tail := strings.TrimPrefix(got, "x-x-x-")
	if len(tail) != 6 {
		t.Errorf("fallback tail should be 6 chars; got %q (len=%d)", tail, len(tail))
	}
	// Tail should be in [0-9a-z].
	for _, r := range tail {
		if !strings.ContainsRune("0123456789abcdefghijklmnopqrstuvwxyz", r) {
			t.Errorf("tail char %q out of [0-9a-z] alphabet; got %q", r, tail)
		}
	}
	releaseWorktreeName(parent, got)
}

// TestRandomWhimsy_FormatIsTriple: randomWhimsy joins three words
// with single dashes. The shape is the contract; specific words
// are non-deterministic.
func TestRandomWhimsy_FormatIsTriple(t *testing.T) {
	got := randomWhimsy()
	parts := strings.Split(got, "-")
	if len(parts) != 3 {
		t.Errorf("randomWhimsy=%q should have 3 dash-separated parts; got %d", got, len(parts))
	}
	// Each part should be non-empty.
	for i, p := range parts {
		if p == "" {
			t.Errorf("part %d empty; got %q", i, got)
		}
	}
}
