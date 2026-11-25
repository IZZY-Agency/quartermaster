package launch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// A launch that fails after the terminal may have opened the session says so
// (ErrMaybeStarted); one the terminal refused does not.
func TestSpawnMaybeStarted(t *testing.T) {
	killed := exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	refused := exec.Command("/bin/sh", "-c", "exit 1").Run()
	usage := exec.Command("/bin/sh", "-c", "exit 64").Run()
	said := func(msg string, err error) error { return fmt.Errorf("agtermctl: %s: %w", msg, err) } // as Exec words it
	spec := LaunchSpec{Name: "w", Cwd: "/tmp", Script: "/tmp/w.sh", WorkspaceName: "ws"}
	for name, c := range map[string]struct {
		out   string
		err   error
		maybe bool
	}{
		"killed":     {err: killed, maybe: true},
		"lost reply": {err: said("Error: read failed", refused), maybe: true},
		"no message": {err: refused, maybe: true},
		"refused":    {err: said(`{"error":"no such workspace","ok":false}`, refused)},
		"stopped":    {err: said("Error: connect(/s) failed: No such file or directory", refused)},
		"usage":      {err: said("Error: Unknown option", usage)},
		"not there":  {err: exec.ErrNotFound},
		"empty":      {out: `{}`, maybe: true},
		"no id":      {out: `{"ok":true,"result":{}}`, maybe: true},
		"not json":   {out: "opened", maybe: true},
		"ok: false":  {out: `{"ok":false,"error":"no such workspace"}`},
	} {
		run := func(context.Context, string, ...string) ([]byte, error) { return []byte(c.out), c.err }
		if _, err := (Agterm{Bin: "agtermctl", Run: run}).Spawn(context.Background(), spec); err == nil || errors.Is(err, ErrMaybeStarted) != c.maybe {
			t.Errorf("agterm, %s: %v, want maybe started %v", name, err, c.maybe)
		}
	}
	for name, c := range map[string]struct {
		err   error
		maybe bool
	}{"killed": {killed, true}, "refused": {refused, false}, "no id": {nil, true}} {
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "has-session" {
				return nil, nil
			}
			return nil, c.err
		}
		if _, err := (Tmux{Bin: "tmux", Run: run}).Spawn(context.Background(), spec); err == nil || errors.Is(err, ErrMaybeStarted) != c.maybe {
			t.Errorf("tmux, %s: %v, want maybe started %v", name, err, c.maybe)
		}
	}
}
