package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// incompleteDir is a Claude folder with an empty sessions folder, and the
// waits between the reads of a half-written file replaced by onWait.
func incompleteDir(t *testing.T, onWait func()) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := liveWait
	t.Cleanup(func() { liveWait = old })
	liveWait = func(context.Context) {
		if onWait != nil {
			onWait()
		}
	}
	return dir
}

func writeSession(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, "sessions", name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sessionJSON(pid int, id string) string {
	return fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":"/w"}`, pid, id)
}

// wantIncomplete fails unless LiveComplete reports path as not read, with
// the sessions it did read still returned.
func wantIncomplete(t *testing.T, dir string, procs []Proc, path string) (map[string]LiveSession, *IncompleteError) {
	t.Helper()
	got, err := LiveComplete(context.Background(), dir, procs)
	var inc *IncompleteError
	if !errors.As(err, &inc) {
		t.Fatalf("LiveComplete err = %v, want an IncompleteError", err)
	}
	if len(inc.Unread) != 1 || inc.Unread[0].Path != path || !strings.Contains(err.Error(), path) {
		t.Fatalf("LiveComplete err = %v (%+v), want it to name %s", err, inc.Unread, path)
	}
	return got, inc
}

// A session file that cannot be read, of a running claude process, makes the
// read incomplete; Live goes on without it.
func TestLiveCompleteUnreadableFileOfRunningClaude(t *testing.T) {
	dir := incompleteDir(t, nil)
	writeSession(t, dir, "202.json", sessionJSON(202, id202))
	// A folder in the file's place cannot be read as a file, root or not.
	path := filepath.Join(dir, "sessions", "101.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	procs := []Proc{{PID: 101, Cmd: "claude"}, {PID: 202, Cmd: "claude"}}
	got, inc := wantIncomplete(t, dir, procs, path)
	if got[id202].PID != 202 || len(got) != 1 {
		t.Errorf("LiveComplete = %+v, want the readable session", got)
	}
	if inc.Unread[0].PID != 101 || !strings.Contains(inc.Error(), "101") {
		t.Errorf("unread = %+v, want process 101 named", inc.Unread)
	}
	live, err := Live(dir, procs)
	if err != nil || live[id202].PID != 202 || len(live) != 1 {
		t.Errorf("Live = %+v, %v, want the readable session and no error", live, err)
	}
}

// The same for a file without read permission.
func TestLiveCompleteNoPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	dir := incompleteDir(t, func() { t.Error("waited to read a file again that needs a permission") })
	path := writeSession(t, dir, "101.json", sessionJSON(101, id101))
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, inc := wantIncomplete(t, dir, []Proc{{PID: 101, Cmd: "claude"}}, path)
	if !errors.Is(inc, os.ErrPermission) || !strings.Contains(inc.Error(), "readable") {
		t.Errorf("err = %v, want the permission error and how to recover", inc)
	}
}

// An unreadable file of a process that is not running, or is not claude, is
// absence: the pid is the file's name.
func TestLiveCompleteUnreadableFileOfDeadProcess(t *testing.T) {
	for _, procs := range [][]Proc{
		{{PID: 5, Cmd: "claude"}},
		{{PID: 101, Cmd: "/bin/zsh"}},
		nil,
	} {
		dir := incompleteDir(t, func() { t.Error("waited for the file of a process that is not claude") })
		writeSession(t, dir, "101.json", `{"pid":10`)
		if got, err := LiveComplete(context.Background(), dir, procs); err != nil || len(got) != 0 {
			t.Errorf("procs %v: LiveComplete = %+v, %v, want nothing live and no error", procs, got, err)
		}
	}
}

// A file whose name is no pid may be any process's: unreadable, it makes the
// read incomplete.
func TestLiveCompleteUnreadableFileWithoutPIDName(t *testing.T) {
	dir := incompleteDir(t, nil)
	path := writeSession(t, dir, "x.json", "{")
	_, inc := wantIncomplete(t, dir, nil, path)
	if inc.Unread[0].PID != 0 || !strings.Contains(inc.Error(), "remove") {
		t.Errorf("err = %v, want no process and how to recover", inc)
	}
}

// A readable file without a pid name that names no session is none.
func TestLiveCompleteOtherFile(t *testing.T) {
	dir := incompleteDir(t, nil)
	writeSession(t, dir, "index.json", `{"version":1}`)
	if got, err := LiveComplete(context.Background(), dir, []Proc{{PID: 101, Cmd: "claude"}}); err != nil || len(got) != 0 {
		t.Errorf("LiveComplete = %+v, %v, want nothing live and no error", got, err)
	}
}

// A file that is gone when it is read is absence (a dangling link reads as
// one that vanished after the listing).
func TestLiveCompleteVanishedFile(t *testing.T) {
	dir := incompleteDir(t, nil)
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "sessions", "101.json")); err != nil {
		t.Fatal(err)
	}
	if got, err := LiveComplete(context.Background(), dir, []Proc{{PID: 101, Cmd: "claude"}}); err != nil || len(got) != 0 {
		t.Errorf("LiveComplete = %+v, %v, want nothing live and no error", got, err)
	}
}

// A half-written file is read again, and one that vanishes meanwhile is
// absence.
func TestLiveCompleteHalfWritten(t *testing.T) {
	procs := []Proc{{PID: 101, Cmd: "claude"}}
	for name, tc := range map[string]struct {
		first string
		then  func(path string) error
		live  bool
	}{
		"truncated":  {`{"pid":101,"sess`, func(p string) error { return os.WriteFile(p, []byte(sessionJSON(101, id101)), 0o600) }, true},
		"empty":      {"", func(p string) error { return os.WriteFile(p, []byte(sessionJSON(101, id101)), 0o600) }, true},
		"no session": {`{"pid":101}`, func(p string) error { return os.WriteFile(p, []byte(sessionJSON(101, id101)), 0o600) }, true},
		"vanished":   {"", os.Remove, false},
	} {
		t.Run(name, func(t *testing.T) {
			var path string
			waits := 0
			dir := incompleteDir(t, func() {
				waits++
				if err := tc.then(path); err != nil {
					t.Fatal(err)
				}
			})
			path = writeSession(t, dir, "101.json", tc.first)
			got, err := LiveComplete(context.Background(), dir, procs)
			if err != nil || waits != 1 {
				t.Fatalf("err = %v after %d waits, want none after one", err, waits)
			}
			if _, ok := got[id101]; ok != tc.live {
				t.Errorf("LiveComplete = %+v, want live %v", got, tc.live)
			}
		})
	}
}

// One that stays half-written, or names no session or another process, is
// not read, after a few tries only.
func TestLiveCompleteStaysBroken(t *testing.T) {
	for _, content := range []string{"", `{"pid":101,"sess`, `{"pid":101}`, `{}`, `[]`, sessionJSON(7, id101)} {
		waits := 0
		dir := incompleteDir(t, func() { waits++ })
		path := writeSession(t, dir, "101.json", content)
		_, _ = wantIncomplete(t, dir, []Proc{{PID: 101, Cmd: "claude"}}, path)
		if waits != liveRetries {
			t.Errorf("content %q: %d waits, want %d", content, waits, liveRetries)
		}
	}
}

// A context that ends while a half-written file is waited for stops the read.
func TestLiveCompleteWaitStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := incompleteDir(t, cancel)
	writeSession(t, dir, "101.json", "{")
	if _, err := LiveComplete(ctx, dir, []Proc{{PID: 101, Cmd: "claude"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A sessions folder that exists but cannot be listed makes the read
// incomplete, with the sessions the arguments name; a missing one does not.
func TestLiveCompleteFolder(t *testing.T) {
	procs := []Proc{{PID: 101, Cmd: "claude --resume " + id101}}
	dir := t.TempDir()
	if got, err := LiveComplete(context.Background(), dir, procs); err != nil || got[id101].PID != 101 {
		t.Errorf("missing folder: LiveComplete = %+v, %v, want the arguments' session", got, err)
	}
	path := filepath.Join(dir, "sessions")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := wantIncomplete(t, dir, procs, path); got[id101].PID != 101 {
		t.Errorf("LiveComplete = %+v, want the arguments' session", got)
	}
	if got, err := Live(dir, procs); err != nil || got[id101].PID != 101 {
		t.Errorf("Live = %+v, %v, want the arguments' session and no error", got, err)
	}
}

// A folder name with glob characters is listed like any other.
func TestLiveFolderWithGlobCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a[b")
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSession(t, dir, "101.json", sessionJSON(101, id101))
	procs := []Proc{{PID: 101, Cmd: "claude"}}
	if got, err := Live(dir, procs); err != nil || got[id101].PID != 101 {
		t.Errorf("Live = %+v, %v, want the session", got, err)
	}
}

// What Live returned stays as it was for the file of a reused pid: a
// readable file that names an earlier process is absence, not incompleteness.
func TestLiveCompleteReusedPID(t *testing.T) {
	dir, _ := claudeDir(t)
	procs := []Proc{{PID: 101, Start: "Sat Nov 21 09:00:00 2026", Cmd: "claude"}}
	if got, err := LiveComplete(context.Background(), dir, procs); err != nil || len(got) != 0 {
		t.Errorf("LiveComplete = %+v, %v, want nothing live and no error", got, err)
	}
}

// A file that contradicts its name is not gone by: it neither adds a session
// nor replaces the process another file gives one.
func TestLiveCompleteMismatchedFileIsNotUsed(t *testing.T) {
	procs := []Proc{{PID: 20, Cmd: "claude"}, {PID: 101, Cmd: "claude"}}
	dir := incompleteDir(t, nil)
	writeSession(t, dir, "101.json", sessionJSON(101, id101))
	writeSession(t, dir, "999.json", sessionJSON(20, id101)) // 999 does not run
	got, err := LiveComplete(context.Background(), dir, procs)
	if err != nil || got[id101].PID != 101 {
		t.Errorf("dead name: LiveComplete = %+v, %v, want process 101's own session", got, err)
	}
	path := writeSession(t, dir, "20.json", sessionJSON(101, id202))
	got, _ = wantIncomplete(t, dir, procs, path)
	if len(got) != 1 || got[id101].PID != 101 {
		t.Errorf("running name: LiveComplete = %+v, want only process 101's own session", got)
	}
}

// A claude run by its version's path is a running claude, though IsClaude
// does not say so: its unreadable file, its file and its arguments count.
func TestLiveCompleteVersionedExecutable(t *testing.T) {
	const exe = "/opt/a b/share/claude/versions/2.1.0"
	dir := incompleteDir(t, nil)
	path := filepath.Join(dir, "sessions", "101.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	wantPID := func(procs []Proc, id string) {
		t.Helper()
		if got, err := LiveComplete(context.Background(), dir, procs); err != nil || got[id].PID != 101 {
			t.Errorf("LiveComplete = %+v, %v, want %s in process 101", got, err, id)
		}
	}
	_, _ = wantIncomplete(t, dir, []Proc{{PID: 101, Cmd: exe + " --permission-mode plan"}}, path)
	for _, cmd := range []string{"/bin/ls " + exe, "grep " + exe, "/bin/sh -c " + exe, "/opt/versions/2.1.0"} {
		if got, err := LiveComplete(context.Background(), dir, []Proc{{PID: 101, Cmd: cmd}}); err != nil || len(got) != 0 {
			t.Errorf("%q: LiveComplete = %+v, %v, want no claude", cmd, got, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	wantPID([]Proc{{PID: 101, Cmd: exe + " --resume " + id202}}, id202)
	writeSession(t, dir, "101.json", sessionJSON(101, id101))
	wantPID([]Proc{{PID: 101, Cmd: exe}}, id101)
}

// A start time that cannot be compared does not prove the pid was reused.
func TestLiveCompleteUnparsableStart(t *testing.T) {
	dir := incompleteDir(t, nil)
	path := writeSession(t, dir, "101.json", fmt.Sprintf(`{"pid":101,"sessionId":%q,"procStart":"12345"}`, id101))
	_, _ = wantIncomplete(t, dir, []Proc{{PID: 101, Start: "Fri Nov 20 09:00:00 2026", Cmd: "claude"}}, path)
	if got, err := LiveComplete(context.Background(), dir, nil); err != nil || len(got) != 0 {
		t.Errorf("dead: LiveComplete = %+v, %v, want nothing live and no error", got, err)
	}
	if got, err := LiveComplete(context.Background(), dir, []Proc{{PID: 101, Start: "12345", Cmd: "claude"}}); err != nil || got[id101].PID != 101 {
		t.Errorf("same text: LiveComplete = %+v, %v, want the session", got, err)
	}
}
