package portal

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	store, err := OpenStore(t.TempDir(), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return NewService(store, fixtureProvider(t, "USD", "1250000"))
}
func input() SourceInput {
	return SourceInput{Name: "Fixture", Origin: "https://vendor.example", PersonalToken: "personal-fixture", Threshold: "3", ThresholdUnit: "USD"}
}
func TestStoreEncryptedDurableAndLocked(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	st, err := OpenStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, fixtureProvider(t, "USD", "1"))
	src, err := svc.Save("", input(), 9)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(st.path)
	if err != nil || strings.Contains(string(raw), "personal-fixture") {
		t.Fatalf("plaintext or read error %v", err)
	}
	info, _ := os.Stat(st.path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if other, err := OpenStore(dir, key); err == nil {
		other.Close()
		t.Fatal("multiple owners allowed")
	}
	st.Close()
	if other, err := OpenStore(dir, bytes.Repeat([]byte{8}, 32)); err == nil {
		other.Close()
		t.Fatal("wrong key accepted")
	}
	st, err = OpenStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec := st.snapshot()[src.ID]
	plain, err := st.decrypt(rec)
	if err != nil || plain != "personal-fixture" {
		t.Fatal(err)
	}
	rec.Source.Origin = "https://other.example"
	if _, err = st.decrypt(rec); err == nil {
		t.Fatal("ciphertext accepted under wrong origin")
	}
}
func TestStoreFailureDoesNotAlterInMemoryState(t *testing.T) {
	svc := newTestService(t)
	st := svc.Store
	st.path = st.path + "/missing/file"
	if _, err := svc.Save("", input(), 9); err == nil {
		t.Fatal("failed write accepted")
	}
	if len(svc.List()) != 0 {
		t.Fatal("uncommitted state exposed")
	}
}
func TestStoreConcurrentSaveAndCAS(t *testing.T) {
	svc := newTestService(t)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.Save("", input(), 9); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(svc.List()) != 10 {
		t.Fatal("lost writes")
	}
	src := svc.List()[0]
	in := input()
	in.Revision = src.Revision
	in.PersonalToken = ""
	in.Name = "new"
	if _, err := svc.Save(src.ID, in, 9); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(src.ID, in, 9); err != ErrConflict {
		t.Fatalf("stale write %v", err)
	}
}
