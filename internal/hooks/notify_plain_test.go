package hooks

import (
	"context"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/config"
)

// No control character of a name reaches the user's notifier.
func TestNotifyStripsControlCharacters(t *testing.T) {
	old := runCommand
	t.Cleanup(func() { runCommand = old })
	var argv []string
	runCommand = func(_ context.Context, a []string) error { argv = a; return nil }
	if err := NotifyContext(context.Background(), config.Config{Notify: []string{"send"}}, "w\x1b[2J\n1", "stopped\r\x07\u202e"); err != nil {
		t.Fatal(err)
	}
	if want := "w[2J 1: stopped "; len(argv) != 2 || argv[1] != want {
		t.Errorf("argv = %q, want the message %q", argv, want)
	}
}
