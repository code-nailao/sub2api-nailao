package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sources.local/portal/internal/portal"
)

const (
	testRunID    = "0123456789abcdef0123456789abcdef"
	testKeyHex   = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	testOtherKey = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
)

// checkFixture is a complete, valid configuration with every optional feature
// enabled and every state file present.
type checkFixture struct {
	root, dir string
	env       map[string]string
}

func (f *checkFixture) getenv(name string) string { return f.env[name] }

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func newCheckFixture(t *testing.T) *checkFixture {
	t.Helper()
	root := t.TempDir()
	f := &checkFixture{root: root, dir: filepath.Join(root, "state")}
	writeFile(t, filepath.Join(root, "master.key"), testKeyHex+"\n", 0600)
	writeFile(t, filepath.Join(root, "bridge.key"), testOtherKey+"\n", 0600)
	writeFile(t, filepath.Join(root, "homeweb", "index.html"), "<!doctype html>", 0644)
	writeFile(t, filepath.Join(root, "studio.json"), `[{"group_id":101,"engine":"sub2api","models":["gpt-image-2"]},{"group_id":102,"engine":"media","models":["gpt-image-2"],"account_ids":[201]}]`, 0600)
	writeFile(t, filepath.Join(f.dir, "sources.json"), `{"version":1,"records":{}}`, 0600)
	writeFile(t, filepath.Join(f.dir, "account-bindings.json"), `{"version":1,"bindings":{}}`, 0600)
	writeFile(t, filepath.Join(f.dir, "studio-bindings.json"), `{}`, 0600)
	writeFile(t, filepath.Join(f.dir, "studio-registry.json"), `{"revision":1,"groups":[]}`, 0600)
	writeFile(t, filepath.Join(f.dir, "studio-custom.json"), `{}`, 0600)
	// A state a start would migrate (no viewer policy) and rewrite (an
	// interrupted submission). The check must accept it and write nothing.
	writeFile(t, filepath.Join(f.dir, "intelligence", "state.json"), fmt.Sprintf(`{"version":1,"revision":3,"admin_only":false,"viewer_access":"authenticated","plans":{},"runs":{%q:{"run":{"id":%q,"plan_id":%q,"benchmark":"candy","status":"submitting","started_at":"2026-01-01T00:00:00Z"}}},"requests":{}}`, testRunID, testRunID, testRunID), 0600)
	if err := os.MkdirAll(filepath.Join(f.dir, "intelligence", "results"), 0700); err != nil {
		t.Fatal(err)
	}
	content, err := portal.OpenContentStore(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := content.Change("publish", 1, 0, 1, nil); err != nil {
		t.Fatal(err)
	}
	f.env = map[string]string{
		"SUB_ORIGIN":                   "https://main.example",
		"PORTAL_ORIGIN":                "https://main.example",
		"PORTAL_KEY_FILE":              filepath.Join(root, "master.key"),
		"PORTAL_DATA_DIR":              f.dir,
		"PORTAL_HOME_DIR":              filepath.Join(root, "homeweb"),
		"PORTAL_LISTEN":                "127.0.0.1:18090",
		"INTELLIGENCE_PROVIDER_ORIGIN": "https://detector.example",
		"SUB_INTERNAL_ORIGIN":          "http://main-internal.example:8080",
		"WALLET_BRIDGE_KEY_FILE":       filepath.Join(root, "bridge.key"),
		"PORTAL_TRUSTED_PROXIES":       "10.0.0.0/8,192.0.2.1/32",
		"STUDIO_CONFIG_FILE":           filepath.Join(root, "studio.json"),
		"STUDIO_URL":                   "https://studio.example/",
		"CANVAS_URL":                   "https://canvas.example/infinite/canvas",
	}
	return f
}

func runCheck(f *checkFixture) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run([]string{checkConfigFlag}, f.getenv, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// treeSnapshot records every entry under root with its mode, size and content
// hash, so any created, removed, rewritten or re-permissioned entry shows up.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		entry := fmt.Sprintf("%v %d", info.Mode(), info.Size())
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			entry += " " + hex.EncodeToString(sum[:])
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestCheckConfigSucceedsWhileStoreIsOpen(t *testing.T) {
	f := newCheckFixture(t)
	key, _ := hex.DecodeString(testKeyHex)
	owner, err := portal.OpenStore(f.dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := portal.OpenStore(f.dir, key); err == nil {
		t.Fatal("second store opened while the lock is held")
	}
	code, stdout, stderr := runCheck(f)
	if code != 0 || stdout != checkConfigOK+"\n" || stderr != "" {
		t.Fatalf("check while locked: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// The real start still fails on the held lock.
	if _, err := prepare(f.getenv, startState); err == nil || err.Error() != errStore {
		t.Fatalf("start while locked: %v", err)
	}
}

func TestCheckConfigSucceedsOnEmptyDataDirectory(t *testing.T) {
	f := newCheckFixture(t)
	empty := filepath.Join(f.root, "empty")
	if err := os.Mkdir(empty, 0700); err != nil {
		t.Fatal(err)
	}
	f.env["PORTAL_DATA_DIR"] = empty
	if code, stdout, stderr := runCheck(f); code != 0 || stdout != checkConfigOK+"\n" || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Fatalf("check created %d entries in an empty data directory", len(entries))
	}
}

func TestCheckConfigDoesNotModifyStateAStartWouldMigrate(t *testing.T) {
	f := newCheckFixture(t)
	before := treeSnapshot(t, f.root)
	if code, _, stderr := runCheck(f); code != 0 {
		t.Fatalf("check failed: %s", stderr)
	}
	if after := treeSnapshot(t, f.root); !sameTree(before, after) {
		t.Fatalf("check modified the tree\nbefore=%v\nafter=%v", before, after)
	}
	// The same state is one a start does rewrite: lock file, viewer migration
	// and the interrupted submission.
	setup, err := prepare(f.getenv, startState)
	if err != nil {
		t.Fatal(err)
	}
	setup.store.Close()
	if after := treeSnapshot(t, f.root); sameTree(before, after) {
		t.Fatal("fixture does not exercise a start-up migration")
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, "intelligence", "state.json"))
	if err != nil || !bytes.Contains(raw, []byte("submission_unknown")) || !bytes.Contains(raw, []byte(`"viewer_policy"`)) {
		t.Fatalf("start did not migrate the fixture: %v", err)
	}
}

func TestCheckConfigRequiresExistingDataDirectory(t *testing.T) {
	f := newCheckFixture(t)
	missing := filepath.Join(f.root, "missing", "state")
	f.env["PORTAL_DATA_DIR"] = missing
	code, stdout, stderr := runCheck(f)
	if code != 1 || stdout != "" || stderr != errDataDirMissing+"\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(f.root, "missing")); !os.IsNotExist(err) {
		t.Fatalf("check created the data directory: %v", err)
	}
	f.env["PORTAL_DATA_DIR"] = filepath.Join(f.root, "master.key")
	if code, _, stderr := runCheck(f); code != 1 || stderr != errDataDirMissing+"\n" {
		t.Fatalf("file as data directory: code=%d stderr=%q", code, stderr)
	}
}

func TestCheckConfigPassesOnReadOnlyState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions, so read-only access cannot be tested")
	}
	f := newCheckFixture(t)
	var dirs []string
	err := filepath.WalkDir(f.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		return os.Chmod(path, 0400)
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0700)
		}
	})
	before := treeSnapshot(t, f.root)
	if code, stdout, stderr := runCheck(f); code != 0 || stdout != checkConfigOK+"\n" {
		t.Fatalf("read-only check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if after := treeSnapshot(t, f.root); !sameTree(before, after) {
		t.Fatal("read-only check changed the tree")
	}
}

func TestCheckConfigRejectsOtherArguments(t *testing.T) {
	for _, args := range [][]string{{"-check-config", "extra"}, {"--check-config"}, {"-check-config=true"}, {"serve"}, {"-h"}, {""}, {"extra", "-check-config"}} {
		var stdout, stderr bytes.Buffer
		reads := 0
		code := run(args, func(string) string { reads++; return "" }, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || stderr.String() != usageLine+"\n" || reads != 0 {
			t.Fatalf("args %q: code=%d stdout=%q stderr=%q env reads=%d", args, code, stdout.String(), stderr.String(), reads)
		}
	}
}

// Every validated variable and file fails the check with the message the
// start gives, through the shared prepare step and the message constants.
func TestCheckConfigFailsLikeStart(t *testing.T) {
	set := func(name, value string) func(*testing.T, *checkFixture) {
		return func(_ *testing.T, f *checkFixture) { f.env[name] = value }
	}
	state := func(name, content string) func(*testing.T, *checkFixture) {
		return func(t *testing.T, f *checkFixture) { writeFile(t, filepath.Join(f.dir, name), content, 0600) }
	}
	file := func(env, content string, mode os.FileMode) func(*testing.T, *checkFixture) {
		return func(t *testing.T, f *checkFixture) { writeFile(t, f.env[env], content, mode) }
	}
	intelligence := func(body string) func(*testing.T, *checkFixture) {
		return state(filepath.Join("intelligence", "state.json"), body)
	}
	cases := []struct {
		name   string
		mutate func(*testing.T, *checkFixture)
		want   string
	}{
		{"sub origin missing", set("SUB_ORIGIN", ""), errSubOrigin},
		{"sub origin http", set("SUB_ORIGIN", "http://main.example"), errSubOrigin},
		{"portal origin missing", set("PORTAL_ORIGIN", ""), errPortalOrigin},
		{"portal origin differs", set("PORTAL_ORIGIN", "https://other.example"), errSameOrigin},
		{"key file missing", set("PORTAL_KEY_FILE", ""), errKeyFile},
		{"key file group readable", file("PORTAL_KEY_FILE", testKeyHex, 0640), errKeyFile},
		{"key file is a directory", func(_ *testing.T, f *checkFixture) { f.env["PORTAL_KEY_FILE"] = f.dir }, errKeyFile},
		{"key not hex", file("PORTAL_KEY_FILE", "not-a-key", 0600), errKeyFormat},
		{"key too short", file("PORTAL_KEY_FILE", testKeyHex[:62], 0600), errKeyFormat},
		{"data dir missing", set("PORTAL_DATA_DIR", ""), errDataDirRequired},
		{"sources corrupt", state("sources.json", "{"), errStore},
		{"sources unsupported version", state("sources.json", `{"version":2,"records":{}}`), errStore},
		{"sources undecryptable", state("sources.json", `{"version":1,"records":{"a":{"source":{"id":"a","origin":"https://up.example"},"encrypted_token":"AAAA"}}}`), errStore},
		{"bindings corrupt", state("account-bindings.json", "[]"), errStore},
		{"bindings unsupported version", state("account-bindings.json", `{"version":2,"bindings":{}}`), errStore},
		{"bindings invalid entry", state("account-bindings.json", `{"version":1,"bindings":{"x":{"main_origin":"https://main.example","account_id":0}}}`), errStore},
		{"lock path is a directory", func(t *testing.T, f *checkFixture) {
			if err := os.Mkdir(filepath.Join(f.dir, "portal.lock"), 0700); err != nil {
				t.Fatal(err)
			}
		}, errStore},
		{"webhook not https", func(_ *testing.T, f *checkFixture) {
			f.env["BALANCE_ALERT_WEBHOOK"] = "http://alerts.example/hook"
			f.env["WALLET_BRIDGE_KEY_FILE"] = ""
		}, errWebhook},
		{"intelligence corrupt", intelligence("{"), errIntelligenceStore},
		{"intelligence unsupported version", intelligence(`{"version":2,"plans":{},"runs":{},"requests":{}}`), errIntelligenceStore},
		{"intelligence null plans", intelligence(`{"version":1,"plans":null,"runs":{},"requests":{}}`), errIntelligenceStore},
		{"intelligence bad run id", intelligence(`{"version":1,"plans":{},"runs":{"x":{"run":{"id":"x"}}},"requests":{}}`), errIntelligenceStore},
		{"intelligence unsupported viewer policy", intelligence(`{"version":1,"viewer_policy":{"version":2,"access":"admin"},"plans":{},"runs":{},"requests":{}}`), errIntelligenceStore},
		{"intelligence plan key mismatch", intelligence(fmt.Sprintf(`{"version":1,"plans":{%q:{"plan":{"id":%q},"encrypted_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}},"runs":{},"requests":{}}`, testRunID, testRunID)), errIntelligenceStore},
		{"intelligence path is a file", func(t *testing.T, f *checkFixture) {
			if err := os.RemoveAll(filepath.Join(f.dir, "intelligence")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(f.dir, "intelligence"), "", 0600)
		}, errIntelligenceStore},
		{"intelligence results is a file", func(t *testing.T, f *checkFixture) {
			if err := os.RemoveAll(filepath.Join(f.dir, "intelligence", "results")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(f.dir, "intelligence", "results"), "", 0600)
		}, errIntelligenceStore},
		{"provider origin missing", set("INTELLIGENCE_PROVIDER_ORIGIN", ""), errIntelligenceOrigin},
		{"provider origin private", set("INTELLIGENCE_PROVIDER_ORIGIN", "https://127.0.0.1"), errIntelligenceOrigin},
		{"api origin overrides invalid", set("INTELLIGENCE_API_ORIGIN_OVERRIDES", "["), errIntelligence},
		{"home dir without assets", func(t *testing.T, f *checkFixture) { f.env["PORTAL_HOME_DIR"] = f.root }, errHomeDir},
		{"home corrupt", state("home.json", "{"), errHome},
		{"home without versions", state("home.json", `{"revision":1,"versions":[]}`), errHome},
		{"internal origin https", set("SUB_INTERNAL_ORIGIN", "https://main-internal.example"), errInternalOrigin},
		{"internal origin with path", set("SUB_INTERNAL_ORIGIN", "http://main-internal.example/x"), errInternalOrigin},
		{"bridge key missing", func(_ *testing.T, f *checkFixture) { f.env["WALLET_BRIDGE_KEY_FILE"] = filepath.Join(f.root, "none") }, errBridgeKey},
		{"bridge key group readable", file("WALLET_BRIDGE_KEY_FILE", testOtherKey, 0640), errBridgeKey},
		{"bridge key not hex", file("WALLET_BRIDGE_KEY_FILE", "nope", 0600), errBridgeConfig},
		{"bridge without internal origin", set("SUB_INTERNAL_ORIGIN", ""), errBridgeConfig},
		{"bridge with webhook", set("BALANCE_ALERT_WEBHOOK", "https://alerts.example/hook"), errBridgeOwner},
		{"trusted proxy invalid", set("PORTAL_TRUSTED_PROXIES", "10.0.0.0/8,nope"), errTrustedProxy},
		{"studio API labels invalid", set("STUDIO_CUSTOM_API_LABELS", "api,API"), errStudioAPILabels},
		{"studio API labels duplicate", set("STUDIO_CUSTOM_API_LABELS", "api,api"), errStudioAPILabels},
		{"studio registry labels invalid", state("studio-registry.json", `{"revision":1,"groups":[],"custom_api_labels":["api"],"custom_api_default":"api2"}`), errStudio},
		{"studio config missing", func(_ *testing.T, f *checkFixture) { f.env["STUDIO_CONFIG_FILE"] = filepath.Join(f.root, "none.json") }, errStudioConfigRead},
		{"studio config corrupt", file("STUDIO_CONFIG_FILE", "{", 0600), errStudioConfig},
		{"studio config unknown engine", file("STUDIO_CONFIG_FILE", `[{"group_id":1,"engine":"other","models":["m"]}]`, 0600), errStudio},
		{"studio legacy engine name invalid", set("SOURCES_MEDIA_ENGINE_LEGACY_NAME", "sub2api"), errStudio},
		{"studio bindings corrupt", state("studio-bindings.json", "null"), errStudio},
		{"studio registry corrupt", state("studio-registry.json", "{"), errStudio},
		{"studio registry unsupported revision", state("studio-registry.json", `{"revision":0,"groups":[]}`), errStudio},
		{"studio custom corrupt", state("studio-custom.json", `{"x":{}}`), errStudio},
		{"studio url missing", set("STUDIO_URL", ""), errStudioURL},
		{"studio url same origin", set("STUDIO_URL", "https://main.example/studio"), errStudioOrigin},
		{"canvas url same origin", set("CANVAS_URL", "https://main.example/canvas"), errCanvasURL},
		{"canvas url http", set("CANVAS_URL", "http://canvas.example/"), errCanvasURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := newCheckFixture(t)
			tc.mutate(t, start)
			_, startErr := prepare(start.getenv, startState)
			check := newCheckFixture(t)
			tc.mutate(t, check)
			code, stdout, stderr := runCheck(check)
			if startErr == nil || startErr.Error() != tc.want {
				t.Fatalf("start: got %v, want %q", startErr, tc.want)
			}
			if code != 1 || stdout != "" || stderr != startErr.Error()+"\n" {
				t.Fatalf("check: code=%d stdout=%q stderr=%q, start said %q", code, stdout, stderr, startErr)
			}
		})
	}
}

func TestCheckConfigListenAddressFailsLikeStart(t *testing.T) {
	for _, addr := range []string{"nonsense", "127.0.0.1:99999", "127.0.0.1:no-such-service-name"} {
		t.Run(addr, func(t *testing.T) {
			start := newCheckFixture(t)
			start.env["PORTAL_LISTEN"] = addr
			var logged bytes.Buffer
			if code := serve(start.getenv, log.New(&logged, "", 0)); code != 1 || strings.TrimSpace(logged.String()) != errListener {
				t.Fatalf("start: code=%d log=%q", code, logged.String())
			}
			check := newCheckFixture(t)
			check.env["PORTAL_LISTEN"] = addr
			if code, _, stderr := runCheck(check); code != 1 || stderr != errListener+"\n" {
				t.Fatalf("check: code=%d stderr=%q", code, stderr)
			}
		})
	}
	f := newCheckFixture(t)
	f.env["PORTAL_LISTEN"] = ""
	if code, _, stderr := runCheck(f); code != 0 {
		t.Fatalf("default listen address: %s", stderr)
	}
}

func TestCheckConfigAcceptsCustomAPILabels(t *testing.T) {
	f := newCheckFixture(t)
	f.env["STUDIO_CUSTOM_API_LABELS"] = "api, api2"
	code, stdout, stderr := runCheck(f)
	if code != 0 || stdout != checkConfigOK+"\n" || stderr != "" {
		t.Fatal(code, stdout, stderr)
	}
}
