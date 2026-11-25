package launch

import (
	"context"
	"slices"
	"testing"
)

// No control character reaches agterm's notification.
func TestAgtermNotifyStripsControlCharacters(t *testing.T) {
	var got []string
	a := Agterm{Bin: "agtermctl", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		got = args
		return []byte(`{}`), nil
	}}
	_ = a.Notify(context.Background(), "", "q\x1bm", "restore: a\nb\x07")
	if want := []string{"notify", "restore: a b", "--title", "qm", "--json"}; !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}
