package main

import (
	"bytes"
	"context"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStartupListenFailureLeavesStateUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		{"occupied", errListener},
		{"invalid", errListenAddress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCheckFixture(t)
			if tc.name == "occupied" {
				occupied, err := net.Listen("tcp", f.env["PORTAL_LISTEN"])
				if err != nil {
					t.Fatal("could not reserve the test listener")
				}
				defer occupied.Close()
				f.env["PORTAL_LISTEN"] = occupied.Addr().String()
				if code, _, stderr := runCheck(f); code != 0 {
					t.Fatalf("read-only check tried to bind the address: %s", stderr)
				}
			} else {
				f.env["PORTAL_LISTEN"] = "invalid-listener"
			}

			state := filepath.Join(f.dir, "intelligence", "state.json")
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			tree := treeSnapshot(t, f.dir)
			var stderr bytes.Buffer
			if code := serve(f.getenv, log.New(&stderr, "", 0)); code != 1 || stderr.String() != tc.want+"\n" {
				t.Fatalf("start: code=%d, stderr=%q, want %q", code, stderr.String(), tc.want)
			}
			after, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("listener failure changed the detection state bytes")
			}
			if !sameTree(tree, treeSnapshot(t, f.dir)) {
				t.Error("listener failure changed the state tree")
			}
		})
	}
}

func TestStartupCreatesMissingDataDirectoryOnlyAfterBinding(t *testing.T) {
	for _, relative := range []string{"fresh-state", filepath.Join("new-parent", "state")} {
		t.Run(relative, func(t *testing.T) {
			f := newCheckFixture(t)
			f.env["PORTAL_DATA_DIR"] = filepath.Join(f.root, relative)
			before := treeSnapshot(t, f.root)
			if code, stdout, stderr := runCheck(f); code != 1 || stdout != "" || stderr != errDataDirMissing+"\n" {
				t.Fatalf("check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !sameTree(before, treeSnapshot(t, f.root)) {
				t.Fatal("read-only check created the data directory")
			}

			addr := f.env["PORTAL_LISTEN"]
			occupied, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatal("could not reserve the test listener")
			}
			defer occupied.Close()
			f.env["PORTAL_LISTEN"] = occupied.Addr().String()
			setup, err := prepare(f.getenv, startState)
			if setup != nil {
				setup.close()
			}
			if err != startupError(errListener) {
				t.Errorf("occupied listener: got %v, want %q", err, errListener)
			}
			if !sameTree(before, treeSnapshot(t, f.root)) {
				t.Error("binding failure created the data directory")
			}

			f.env["PORTAL_LISTEN"] = addr
			setup, err = prepare(f.getenv, startState)
			if err != nil {
				t.Fatalf("first startup: %v", err)
			}
			defer setup.close()
			if info, err := os.Stat(filepath.Join(f.env["PORTAL_DATA_DIR"], "portal.lock")); err != nil || !info.Mode().IsRegular() {
				t.Fatal("first startup did not create the state directory and lock file")
			}
		})
	}
}

func TestStartupRejectsInvalidDataDirectory(t *testing.T) {
	for _, name := range []string{"file", "file-with-trailing-slash", "child-of-file", "fifo", "dangling-symlink", "child-of-dangling-symlink", "symlink-loop"} {
		t.Run(name, func(t *testing.T) {
			f := newCheckFixture(t)
			candidate := filepath.Join(f.root, "candidate")
			switch name {
			case "file", "file-with-trailing-slash", "child-of-file":
				writeFile(t, candidate, "fixture", 0600)
				if name == "file-with-trailing-slash" {
					candidate += string(os.PathSeparator)
				}
				if name == "child-of-file" {
					candidate = filepath.Join(candidate, "state")
				}
			case "fifo":
				if err := syscall.Mkfifo(candidate, 0600); err != nil {
					t.Fatal(err)
				}
			case "dangling-symlink", "child-of-dangling-symlink":
				if err := os.Symlink("missing", candidate); err != nil {
					t.Fatal(err)
				}
				if name == "child-of-dangling-symlink" {
					candidate = filepath.Join(candidate, "state")
				}
			case "symlink-loop":
				if err := os.Symlink(filepath.Base(candidate), candidate); err != nil {
					t.Fatal(err)
				}
			}
			f.env["PORTAL_DATA_DIR"] = candidate
			before := treeSnapshot(t, f.root)

			code, stdout, stderr := runCheck(f)
			if code != 1 || stdout != "" || stderr != errDataDirMissing+"\n" {
				t.Errorf("check: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !sameTree(before, treeSnapshot(t, f.root)) {
				t.Error("read-only check changed the fixture")
			}

			// Exercise the real startup preparation without running any workers
			// if a regression allows the invalid configuration to start.
			setup, err := prepare(f.getenv, startState)
			if setup != nil {
				setup.close()
			}
			if err != startupError(errDataDirMissing) {
				t.Errorf("start: got %v, want %q", err, errDataDirMissing)
			}
			if !sameTree(before, treeSnapshot(t, f.root)) {
				t.Error("start with invalid data directory changed the fixture")
			}
		})
	}
}

func TestStartupRejectsStudioConfigFIFO(t *testing.T) {
	// Run the entry points in a child so a regression cannot leave the suite
	// waiting for a writer on the pipe.
	const modeEnv = "PORTAL_TEST_FIFO_MODE"
	if mode := os.Getenv(modeEnv); mode != "" {
		var stdout, stderr bytes.Buffer
		var code int
		switch mode {
		case "start":
			code = serve(os.Getenv, log.New(&stderr, "", 0))
		case "check":
			code = run([]string{checkConfigFlag}, os.Getenv, &stdout, &stderr)
		default:
			t.Fatal("invalid child test mode")
		}
		if code != 1 || stdout.Len() != 0 || stderr.String() != errStudioConfigRead+"\n" {
			t.Fatalf("Portal did not report the studio configuration error: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		return
	}

	for _, mode := range []string{"start", "check"} {
		t.Run(mode, func(t *testing.T) {
			f := newCheckFixture(t)
			pipe := filepath.Join(f.root, "studio.pipe")
			if err := syscall.Mkfifo(pipe, 0600); err != nil {
				t.Fatal("could not create the test pipe")
			}
			f.env["STUDIO_CONFIG_FILE"] = pipe
			before := treeSnapshot(t, f.root)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupRejectsStudioConfigFIFO$")
			cmd.Env = []string{modeEnv + "=" + mode}
			for name, value := range f.env {
				cmd.Env = append(cmd.Env, name+"="+value)
			}
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Error("Portal blocked on a named pipe without a writer")
			} else if err != nil {
				t.Errorf("Portal config check in the child failed: %v\n%s", err, output)
			}
			if !sameTree(before, treeSnapshot(t, f.root)) {
				t.Error("invalid studio configuration changed the state tree")
			}
		})
	}
}
