package launch

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAgtermNotify(t *testing.T) {
	for _, c := range []struct{ socket, want string }{
		{"", "notify|the body|--title|quartermaster|--json"},
		{"/run/a.sock", "notify|the body|--title|quartermaster|--socket|/run/a.sock|--json"},
	} {
		var got string
		a := Agterm{Bin: "agtermctl", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			got = strings.Join(args, "|")
			return []byte(`{"ok":true,"result":{"text":"badge updated"}}`), nil
		}}
		if err := a.Notify(context.Background(), c.socket, "quartermaster", "the body"); err != nil || got != c.want {
			t.Errorf("ran %q, %v: want %q", got, err, c.want)
		}
	}
	a := Agterm{Bin: "agtermctl", Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"ok":false,"error":"notFound"}`), nil
	}}
	if err := a.Notify(context.Background(), "", "t", "b"); err == nil || !strings.Contains(err.Error(), "notFound") {
		t.Errorf("a refused notification: %v", err)
	}
}

func TestAgtermHooks(t *testing.T) {
	out := `{"ok":true,"result":{"hooks":{"path":"/c/hooks.conf","diagnostics":[{"line":3,"message":"unknown event kind"}],"hooks":[]}}}`
	a := Agterm{Bin: "agtermctl", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if got := strings.Join(args, " "); got != "hooks list --json" {
			t.Errorf("ran %s", got)
		}
		return []byte(out), nil
	}}
	h, err := a.Hooks(context.Background())
	if err != nil || h.Path != "/c/hooks.conf" || len(h.Refused) != 1 || h.Refused[3] != "unknown event kind" {
		t.Errorf("hooks %+v, %v", h, err)
	}
	out = `{"ok":true,"result":{}}`
	if _, err := a.Hooks(context.Background()); err == nil {
		t.Error("no path: want an error")
	}
}

func TestAgtermHooksReload(t *testing.T) {
	a := Agterm{Bin: "agtermctl", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if got := strings.Join(args, " "); got != "hooks reload --json" {
			t.Errorf("ran %s", got)
		}
		return []byte(`{"ok":true,"result":{"count":0}}`), nil
	}}
	if err := a.HooksReload(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestAgtermKnowsEvent(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New("agtermctl: Error: event run changed: exit status 1"), true},
		{errors.New("agtermctl: Error: invalid event kind: app.launched: exit status 64"), false},
		{errors.New("agtermctl: Error: connect(/a.sock) failed"), false},
		{nil, false},
	} {
		var got string
		a := Agterm{Bin: "agtermctl", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			got = strings.Join(args, " ")
			return nil, c.err
		}}
		if a.KnowsEvent(context.Background(), "app.launched") != c.want {
			t.Errorf("%v: want %v", c.err, c.want)
		}
		if got != "events --json --kind app.launched --run 00000000-0000-0000-0000-000000000000 --after 0" {
			t.Errorf("ran %s", got)
		}
	}
}
