package mediaengine

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The legacy values are deliberately fake; only their positions matter.
const (
	testLegacyName       = "legacyname"
	testLegacyTaskPrefix = "legacytask"
)

func mustNamespace(t *testing.T, legacy, taskPrefix string) Namespace {
	t.Helper()
	ns, err := NewNamespace(legacy, taskPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func legacyNamespace(t *testing.T) Namespace {
	return mustNamespace(t, testLegacyName, testLegacyTaskPrefix)
}

func TestNamespaceValues(t *testing.T) {
	for _, tc := range []struct{ legacy, prefix, name, selector, keyCheck, taskPrefix string }{
		{"", "", "media-engine", "media", "sources-media-engine-v1", "mediatask"},
		{testLegacyName, testLegacyTaskPrefix, "legacyname", "legacyname", "sources-legacyname-v1", "legacytask"},
	} {
		ns := mustNamespace(t, tc.legacy, tc.prefix)
		if ns.Legacy() != tc.legacy || ns.Name() != tc.name || ns.Selector() != tc.selector || ns.keyCheck() != tc.keyCheck || ns.TaskPrefix() != tc.taskPrefix {
			t.Fatalf("%q: got %q %q %q %q", tc.legacy, ns.Name(), ns.Selector(), ns.keyCheck(), ns.TaskPrefix())
		}
		hex := strings.Repeat("0a", 16)
		if id := ns.newTaskID(hex); id != tc.taskPrefix+"_"+hex {
			t.Fatalf("task id %q", id)
		}
	}
	if (Namespace{}) != mustNamespace(t, "", "") {
		t.Fatal("the zero namespace must be the neutral one")
	}
}

func TestNamespaceAcceptsBothForms(t *testing.T) {
	legacy, neutral := legacyNamespace(t), mustNamespace(t, "", "")
	for v, want := range map[string]bool{"legacyname": true, "media": true, "media-engine": false, "otherlegacy": false, "": false} {
		if legacy.IsSelector(v) != want {
			t.Fatalf("legacy selector %q", v)
		}
	}
	if neutral.IsSelector("legacyname") || !neutral.IsSelector("media") {
		t.Fatal("neutral selector")
	}
	for v, want := range map[string]bool{"sources-legacyname-v1": true, "sources-media-engine-v1": true, "sources-otherlegacy-v1": false} {
		if legacy.Matches(v, keyCheckValue) != want {
			t.Fatalf("legacy key check %q", v)
		}
	}
	if neutral.Matches("sources-legacyname-v1", keyCheckValue) || !neutral.Matches("sources-media-engine-v1", keyCheckValue) {
		t.Fatal("neutral key check")
	}
	hex := strings.Repeat("a", 32)
	for _, tc := range []struct {
		id              string
		legacy, neutral bool
	}{
		{"legacytask_" + hex, true, false},
		{"mediatask_" + hex, true, true},
		{"othertask_" + hex, false, false},
		{"legacytask_" + hex[:31], false, false},
		{"legacytask_" + strings.ToUpper(hex), false, false},
		{"legacytask" + hex, false, false},
	} {
		if legacy.IsTaskID(tc.id) != tc.legacy || neutral.IsTaskID(tc.id) != tc.neutral {
			t.Fatalf("task id %q", tc.id)
		}
	}
	if !legacy.HasTaskPrefix("legacytask_x") || !legacy.HasTaskPrefix("mediatask_x") || neutral.HasTaskPrefix("legacytask_x") || legacy.HasTaskPrefix("legacytaskx") {
		t.Fatal("task prefix routing")
	}
}

func TestNamespaceRejectsInvalidValuesWithoutEchoing(t *testing.T) {
	for _, tc := range []struct{ name, prefix string }{
		{"Legacyname", "legacytask"}, {"legacy-name", "legacytask"}, {"legacy_name", "legacytask"},
		{" legacyname", "legacytask"}, {"legacyname\n", "legacytask"}, {"x", "legacytask"}, {"sub2api", "legacytask"},
		{strings.Repeat("a", 33), "legacytask"},
		{"legacyname", "Legacytask"}, {"legacyname", "legacy_task"}, {"legacyname", "q"}, {"legacyname", strings.Repeat("b", 17)},
		{"legacyname", ""}, {"", "legacytask"},
	} {
		_, err := NewNamespace(tc.name, tc.prefix)
		if err == nil {
			t.Fatalf("accepted %q/%q", tc.name, tc.prefix)
		}
		for _, v := range []string{tc.name, tc.prefix} {
			if v = strings.TrimSpace(v); v != "" && strings.Contains(err.Error(), v) {
				t.Fatalf("error echoes a private value: %v", err)
			}
		}
	}
}

func TestStoreKeyCheckFollowsNamespace(t *testing.T) {
	key := []byte(strings.Repeat("a", 32))
	reopen := func(dir string, ns Namespace) error {
		s, err := Open(dir, key, ns)
		if err == nil {
			_ = s.Close()
		}
		return err
	}
	create := func(ns Namespace) string {
		dir := filepath.Join(t.TempDir(), "tasks")
		if err := reopen(dir, ns); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	legacy, neutral, other := legacyNamespace(t), mustNamespace(t, "", ""), mustNamespace(t, "otherlegacy", testLegacyTaskPrefix)

	legacyStore := create(legacy)
	if err := reopen(legacyStore, legacy); err != nil {
		t.Fatalf("legacy store under its own name: %v", err)
	}
	if s, err := OpenProducer(legacyStore, key, legacy); err != nil {
		t.Fatalf("legacy producer: %v", err)
	} else {
		_ = s.Close()
	}
	for name, ns := range map[string]Namespace{"neutral": neutral, "other": other} {
		if err := reopen(legacyStore, ns); err == nil || err.Error() != "task store identity or key check failed" {
			t.Fatalf("legacy store opened under %s name: %v", name, err)
		}
		if _, err := OpenProducer(legacyStore, key, ns); err == nil {
			t.Fatalf("legacy producer opened under %s name", name)
		}
	}

	neutralStore := create(neutral)
	if err := reopen(neutralStore, neutral); err != nil {
		t.Fatalf("neutral store: %v", err)
	}
	// Input accepts the neutral form too, so configuring a legacy name on a
	// fresh store is harmless.
	if err := reopen(neutralStore, legacy); err != nil {
		t.Fatalf("neutral store under a legacy name: %v", err)
	}
	if err := reopen(neutralStore, neutral); err != nil {
		t.Fatalf("neutral store unchanged by a legacy open: %v", err)
	}
}

// The stored plaintext is what earlier releases sealed with their name in the
// same position, so an existing store opens without migration.
func TestStoreSealsNamespaceKeyCheck(t *testing.T) {
	key := []byte(strings.Repeat("a", 32))
	for _, tc := range []struct{ legacy, prefix, want string }{{"", "", "sources-media-engine-v1"}, {testLegacyName, testLegacyTaskPrefix, "sources-legacyname-v1"}} {
		s, err := Open(filepath.Join(t.TempDir(), "tasks"), key, mustNamespace(t, tc.legacy, tc.prefix))
		if err != nil {
			t.Fatal(err)
		}
		var sealed []byte
		if err = s.db.QueryRow("SELECT key_check FROM store_metadata WHERE id=1").Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		raw, err := s.open("key-check", sealed)
		if err != nil || string(raw) != tc.want {
			t.Fatalf("%q: sealed %q, %v", tc.legacy, raw, err)
		}
		_ = s.Close()
	}
}

// Producers generate ids with the namespace's prefix, and a store keeps
// serving ids with the legacy prefix only while that prefix is configured.
func TestTaskIDsFollowNamespace(t *testing.T) {
	key := []byte(strings.Repeat("a", 32))
	for _, tc := range []struct {
		ns   Namespace
		want string
	}{{mustNamespace(t, "", ""), `^mediatask_[a-f0-9]{32}$`}, {legacyNamespace(t), `^legacytask_[a-f0-9]{32}$`}} {
		dir := filepath.Join(t.TempDir(), "tasks")
		s, err := Open(dir, key, tc.ns)
		if err != nil {
			t.Fatal(err)
		}
		p, err := NewProducer(s, Options{})
		if err != nil {
			t.Fatal(err)
		}
		task, err := p.Create(input())
		if err != nil || !regexp.MustCompile(tc.want).MatchString(task.ID) {
			t.Fatalf("generated %v, %v", task, err)
		}
		if got, err := p.Get(task.Owner, task.ID); err != nil || got.ID != task.ID {
			t.Fatalf("read back %v, %v", got, err)
		}
		_ = s.Close()
	}

	// A store that already holds a legacy-prefixed task: same legacy name (so the
	// key check passes), with and without the matching task prefix.
	dir := filepath.Join(t.TempDir(), "tasks")
	s, err := Open(dir, key, legacyNamespace(t))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := NewProducer(s, Options{})
	task, err := p.Create(input())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	for _, tc := range []struct {
		prefix string
		found  bool
	}{{testLegacyTaskPrefix, true}, {"othertask", false}} {
		s, err := Open(dir, key, mustNamespace(t, testLegacyName, tc.prefix))
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(task.ID)
		if tc.found && (err != nil || got.ID != task.ID) {
			t.Fatalf("legacy task unreadable with its prefix: %v", err)
		}
		if !tc.found && !errors.Is(err, ErrNotFound) {
			t.Fatalf("legacy task readable without its prefix: %v, %v", got, err)
		}
		if !tc.found {
			if err := s.Save(task); !errors.Is(err, ErrConflict) {
				t.Fatalf("legacy task writable without its prefix: %v", err)
			}
		}
		_ = s.Close()
	}

	// Operator-supplied recovery scopes must name this store's tasks.
	s, err = Open(filepath.Join(t.TempDir(), "tasks"), key, mustNamespace(t, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	policy := validRecoveryPolicyForNamespaceTest()
	policy.LegacyTaskIDs = []string{"legacytask_" + strings.Repeat("a", 32)}
	if policy.Validate() != nil {
		t.Fatal("a well-formed id fails the shape check")
	}
	if _, err := NewProducer(s, Options{RecoveryPolicy: &policy}); err == nil {
		t.Fatal("a recovery scope with a foreign prefix was accepted")
	}
	policy.LegacyTaskIDs = []string{"mediatask_" + strings.Repeat("a", 32)}
	if _, err := NewProducer(s, Options{RecoveryPolicy: &policy}); err != nil {
		t.Fatal(err)
	}
}

func validRecoveryPolicyForNamespaceTest() RecoveryPolicy {
	return RecoveryPolicy{ID: "test-v1", EffectiveFrom: 1, TimeoutSeconds: 3600, MaxReleaseAmount: "0.10", MaxClosuresPerAccount24h: 3}
}
