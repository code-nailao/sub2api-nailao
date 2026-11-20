package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkbenchURLsRejectCanonicalMainOrigin(t *testing.T) {
	for _, variable := range []string{"STUDIO_URL", "CANVAS_URL"} {
		for _, value := range []string{
			"https://MAIN.EXAMPLE/workbench",
			"HTTPS://main.example/workbench",
			"https://main.example:443/workbench",
			"HTTPS://MAIN.EXAMPLE:443/workbench",
		} {
			t.Run(variable+"/"+value, func(t *testing.T) {
				f := newCheckFixture(t)
				f.env[variable] = value
				want := errStudioOrigin
				if variable == "CANVAS_URL" {
					want = errCanvasURL
				}
				if code, stdout, stderr := runCheck(f); code != 1 || stdout != "" || stderr != want+"\n" {
					t.Errorf("check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				setup, err := prepare(f.getenv, startState)
				if setup != nil {
					setup.store.Close()
				}
				if err == nil || err.Error() != want {
					t.Fatalf("start: got %v, want %q", err, want)
				}
			})
		}
	}
}

func TestWorkbenchURLsAllowSeparateCanonicalOrigins(t *testing.T) {
	f := newCheckFixture(t)
	f.env["SUB_ORIGIN"] = "HTTPS://MAIN.EXAMPLE:443/"
	f.env["STUDIO_URL"] = "HTTPS://STUDIO.EXAMPLE:443/workbench"
	f.env["CANVAS_URL"] = "HTTPS://CANVAS.EXAMPLE:443/infinite/canvas"
	if code, _, stderr := runCheck(f); code != 0 {
		t.Fatalf("check failed: %s", stderr)
	}
	setup, err := prepare(f.getenv, startState)
	if err != nil {
		t.Fatal(err)
	}
	setup.store.Close()
}

func TestStudioConfigRequiresArray(t *testing.T) {
	for _, content := range []string{"null", " \n null\t", "[]"} {
		t.Run(content, func(t *testing.T) {
			f := newCheckFixture(t)
			writeFile(t, f.env["STUDIO_CONFIG_FILE"], content, 0600)
			before := treeSnapshot(t, f.root)
			code, stdout, stderr := runCheck(f)
			setup, err := prepare(f.getenv, startState)
			if setup != nil {
				setup.store.Close()
			}
			if content == "[]" {
				if code != 0 || stdout != checkConfigOK+"\n" || stderr != "" || err != nil {
					t.Fatalf("empty array: check=%d stdout=%q stderr=%q start=%v", code, stdout, stderr, err)
				}
				if setup.server.Studio == nil {
					t.Fatal("empty registry disabled the workbench")
				}
				return
			}
			if code != 1 || stdout != "" || stderr != errStudioConfig+"\n" || err == nil || err.Error() != errStudioConfig {
				t.Errorf("null: check=%d stdout=%q stderr=%q start=%v", code, stdout, stderr, err)
			}
			if !sameTree(before, treeSnapshot(t, f.root)) {
				t.Error("invalid registry changed the state tree")
			}
		})
	}
}

func TestInvalidStartupConfigDoesNotModifyState(t *testing.T) {
	set := func(name, value string) func(*testing.T, *checkFixture) {
		return func(_ *testing.T, f *checkFixture) { f.env[name] = value }
	}
	file := func(name, content string, mode os.FileMode) func(*testing.T, *checkFixture) {
		return func(t *testing.T, f *checkFixture) { writeFile(t, f.env[name], content, mode) }
	}
	cases := []struct {
		name   string
		mutate func(*testing.T, *checkFixture)
		want   string
	}{
		{"main origin", set("SUB_ORIGIN", "http://main.example"), errSubOrigin},
		{"portal origin", set("PORTAL_ORIGIN", "http://main.example"), errPortalOrigin},
		{"different portal origin", set("PORTAL_ORIGIN", "https://other.example"), errSameOrigin},
		{"key file", set("PORTAL_KEY_FILE", ""), errKeyFile},
		{"key permissions", file("PORTAL_KEY_FILE", testKeyHex, 0640), errKeyFile},
		{"key format", file("PORTAL_KEY_FILE", "not-a-key", 0600), errKeyFormat},
		{"data directory", set("PORTAL_DATA_DIR", ""), errDataDirRequired},
		{"webhook", set("BALANCE_ALERT_WEBHOOK", "http://alerts.example/hook"), errWebhook},
		{"missing provider origin", set("INTELLIGENCE_PROVIDER_ORIGIN", ""), errIntelligenceOrigin},
		{"invalid provider origin", set("INTELLIGENCE_PROVIDER_ORIGIN", "http://detector.example"), errIntelligenceOrigin},
		{"overrides JSON", set("INTELLIGENCE_API_ORIGIN_OVERRIDES", "["), errIntelligence},
		{"overrides origin", set("INTELLIGENCE_API_ORIGIN_OVERRIDES", `{"1":"http://api.example"}`), errIntelligence},
		{"home assets", func(_ *testing.T, f *checkFixture) { f.env["PORTAL_HOME_DIR"] = f.root }, errHomeDir},
		{"internal origin", set("SUB_INTERNAL_ORIGIN", "https://internal.example"), errInternalOrigin},
		{"bridge internal origin", set("SUB_INTERNAL_ORIGIN", ""), errBridgeConfig},
		{"bridge permissions", file("WALLET_BRIDGE_KEY_FILE", testOtherKey, 0640), errBridgeKey},
		{"bridge format", file("WALLET_BRIDGE_KEY_FILE", "not-a-key", 0600), errBridgeConfig},
		{"bridge notification owner", set("BALANCE_ALERT_WEBHOOK", "https://alerts.example/hook"), errBridgeOwner},
		{"trusted proxies", set("PORTAL_TRUSTED_PROXIES", "invalid-prefix"), errTrustedProxy},
		{"studio config unavailable", func(_ *testing.T, f *checkFixture) {
			f.env["STUDIO_CONFIG_FILE"] = filepath.Join(f.root, "missing.json")
		}, errStudioConfigRead},
		{"studio config JSON", file("STUDIO_CONFIG_FILE", "{", 0600), errStudioConfig},
		{"studio config null", file("STUDIO_CONFIG_FILE", "null", 0600), errStudioConfig},
		{"studio engine", file("STUDIO_CONFIG_FILE", `[{"group_id":1,"engine":"other","models":["model"]}]`, 0600), errStudio},
		{"legacy media engine", set("SOURCES_MEDIA_ENGINE_LEGACY_NAME", "sub2api"), errStudio},
		{"studio URL", set("STUDIO_URL", ""), errStudioURL},
		{"studio main origin", set("STUDIO_URL", "https://MAIN.EXAMPLE:443/studio"), errStudioOrigin},
		{"canvas URL", set("CANVAS_URL", "http://canvas.example/canvas"), errCanvasURL},
		{"canvas main origin", set("CANVAS_URL", "https://MAIN.EXAMPLE:443/canvas"), errCanvasURL},
		{"listener", set("PORTAL_LISTEN", "invalid-listener"), errListener},
		{"listener service", set("PORTAL_LISTEN", ":invalid-service"), errListener},
	}
	for _, tc := range cases {
		for _, missing := range []bool{false, true} {
			name := tc.name + "/existing state"
			if missing {
				name = tc.name + "/missing state"
			}
			t.Run(name, func(t *testing.T) {
				f := newCheckFixture(t)
				if missing {
					f.env["PORTAL_DATA_DIR"] = filepath.Join(f.root, "missing", "state")
				}
				tc.mutate(t, f)
				before := treeSnapshot(t, f.root)
				setup, err := prepare(f.getenv, startState)
				if setup != nil {
					setup.store.Close()
				}
				if err == nil || err.Error() != tc.want {
					t.Errorf("start: got %v, want %q", err, tc.want)
				}
				if !sameTree(before, treeSnapshot(t, f.root)) {
					t.Error("invalid configuration changed the state tree")
				}
				if code, stdout, stderr := runCheck(f); code != 1 || stdout != "" || stderr != tc.want+"\n" {
					t.Errorf("check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
			})
		}
	}
}

func TestStartupCreatesMissingStateAfterValidation(t *testing.T) {
	f := newCheckFixture(t)
	dir := filepath.Join(f.root, "missing", "state")
	f.env["PORTAL_DATA_DIR"] = dir
	setup, err := prepare(f.getenv, startState)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.store.Close()
	if info, err := os.Stat(filepath.Join(dir, "portal.lock")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("valid start did not create its state lock: %v", err)
	}
}
