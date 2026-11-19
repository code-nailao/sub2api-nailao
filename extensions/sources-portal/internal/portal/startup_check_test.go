package portal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The read-only openers decrypt real records with the same validators as the
// start, report a wrong key, and refuse every write.
func TestCheckStoresValidateEncryptedStateAndRefuseWrites(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	owner, err := OpenStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := NewService(owner, fixtureProvider(t, "USD", "1")).Save("", input(), 9); err != nil {
		t.Fatal(err)
	}
	intelligence, err := OpenIntelligenceStore(owner)
	if err != nil {
		t.Fatal(err)
	}
	plan := IntelligencePlan{ID: "0123456789abcdef0123456789abcdef", GroupID: 5}
	secret, err := intelligence.encrypt(plan, "sk-check-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := intelligence.change(func(st *intelligenceState) error {
		st.Plans[plan.ID] = intelligencePlanRecord{Plan: plan, Secret: secret}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Lock held by owner; the check still reads everything.
	checked, err := CheckStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.snapshot()) != 1 {
		t.Fatal("check store did not load the record")
	}
	checkedIntelligence, err := CheckIntelligenceStore(checked)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckIntelligenceService(checkedIntelligence, "https://main.example", "https://detector.example", ""); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "sources.json"))
	if err := checked.change(func(map[string]record) error { return nil }); !errors.Is(err, ErrPersistenceUncertain) {
		t.Fatalf("check store accepted a write: %v", err)
	}
	if err := checkedIntelligence.change(func(*intelligenceState) error { return nil }); !errors.Is(err, ErrPersistenceUncertain) {
		t.Fatalf("check intelligence store accepted a write: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "sources.json")); !bytes.Equal(before, after) {
		t.Fatal("check store wrote state")
	}
	if checked.Close() != nil {
		t.Fatal("closing a check store must be a no-op")
	}

	other := bytes.Repeat([]byte{8}, 32)
	if _, err := CheckStore(dir, other); err == nil || err.Error() != "store_key_mismatch" {
		t.Fatalf("wrong key: %v", err)
	}
	// Same wrong key on the intelligence state alone: drop the balance record.
	if err := os.Remove(filepath.Join(dir, "sources.json")); err != nil {
		t.Fatal(err)
	}
	wrong, err := CheckStore(dir, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckIntelligenceStore(wrong); err == nil || err.Error() != "intelligence_key_mismatch" {
		t.Fatalf("wrong intelligence key: %v", err)
	}
	if _, err := CheckStore(filepath.Join(dir, "missing"), key); err == nil {
		t.Fatal("check store accepted a missing directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatal("check store created a directory")
	}
}
