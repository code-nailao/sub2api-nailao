package portal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHomeContentDraftPublishRestore(t *testing.T) {
	dir := t.TempDir()
	s, e := OpenContentStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	initial := s.Read()
	c := initial.Draft
	c.Brand.HeroTitle = "草稿 <script>alert(1)</script>" // plain text, rendered by Vue interpolation
	next, e := s.Change("draft", 1, 0, 9, &c)
	if e != nil {
		t.Fatal(e)
	}
	if next.Versions[0].Config.Brand.HeroTitle == c.Brand.HeroTitle {
		t.Fatal("draft leaked")
	}
	if _, e = s.Change("publish", 1, 0, 9, nil); e != ErrConflict {
		t.Fatal("missing CAS")
	}
	next, e = s.Change("publish", next.Revision, 0, 9, nil)
	if e != nil || next.Versions[1].Config.Brand.HeroTitle != c.Brand.HeroTitle {
		t.Fatal(e)
	}
	s, e = OpenContentStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	next, e = s.Change("restore", next.Revision, 1, 9, nil)
	if e != nil {
		t.Fatal(e)
	}
	if next.Draft.Brand.HeroTitle != initial.Draft.Brand.HeroTitle || next.Versions[len(next.Versions)-1].Config.Brand.HeroTitle != c.Brand.HeroTitle {
		t.Fatal("restore must only change draft")
	}
	info, _ := os.Stat(filepath.Join(dir, "home.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("permissions")
	}
	// Old private business fields are rejected by decoder, not silently re-enabled.
	var input HomeContent
	d := json.NewDecoder(strings.NewReader(`{"site_name":"x","subscription":{}}`))
	d.DisallowUnknownFields()
	if d.Decode(&input) == nil {
		t.Fatal("old subscription accepted")
	}
}
func TestHomeContentLinkValidation(t *testing.T) {
	s, e := OpenContentStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	base := s.Read().Draft
	for _, url := range []string{"javascript:alert(1)", "//evil.test", "data:text/html,hi", "https://a:b@evil.test", "/\\evil.test", "http://evil.test"} {
		c := base
		c.DocURL = url
		if validateContent(c) == nil {
			t.Errorf("allowed %q", url)
		}
	}
	for _, path := range []string{"/login?redirect=https://evil.test", "https://evil.test", "/custom/x/../../admin"} {
		c := base
		c.WelfarePath = path
		if validateContent(c) == nil {
			t.Fatal(path)
		}
	}
}

func TestHomeRenameCommittedButSyncFailed(t *testing.T) {
	dir := t.TempDir()
	s, e := OpenContentStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	s.writer = func(path string, v any) error {
		return writeJSONAtomicWithSync(path, v, func(string) error { return os.ErrPermission })
	}
	c := s.Read().Draft
	c.SiteName = "new committed content"
	if _, e = s.Change("draft", 1, 0, 9, &c); e == nil || !writeCommitted(e) {
		t.Fatal("not recorded committed error", e)
	}
	if s.Read().Revision != 2 || s.Read().Draft.SiteName != c.SiteName {
		t.Fatal("memory diverged from renamed file")
	}
	reopened, e := OpenContentStore(dir)
	if e != nil || reopened.Read().Revision != 2 {
		t.Fatal(e)
	}
	if _, e = s.Change("publish", 2, 0, 9, nil); e != ErrPersistenceUncertain {
		t.Fatal("must stop writes after uncertain durability", e)
	}
}

func TestHomeRejectsNullArraysRatherThanPublishingBrokenPage(t *testing.T) {
	s, e := OpenContentStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	c := s.Read().Draft
	c.Brand.Contacts = nil
	if validateContent(c) == nil {
		t.Fatal("null contacts accepted")
	}
	c = s.Read().Draft
	c.Brand.Stats = nil
	if validateContent(c) == nil {
		t.Fatal("null stats accepted")
	}
}
