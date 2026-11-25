package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Hooks is what agterm says of its event hooks: the hooks.conf it reads, and
// the lines of it agterm does not take, with why.
type Hooks struct {
	Path    string
	Refused map[int]string
}

// Hooks runs: agtermctl hooks list --json. An error also means agterm is not
// running, or has no hooks.
func (a Agterm) Hooks(ctx context.Context) (Hooks, error) {
	raw, err := a.call(ctx, "hooks list", "hooks", "list", "--json")
	if err != nil {
		return Hooks{}, fmt.Errorf("agterm: %w", err)
	}
	var res struct {
		Hooks struct {
			Path        string `json:"path"`
			Diagnostics []struct {
				Line    int    `json:"line"`
				Message string `json:"message"`
			} `json:"diagnostics"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.Hooks.Path == "" {
		return Hooks{}, fmt.Errorf("agterm: %s hooks list named no file", a.Bin)
	}
	h := Hooks{Path: res.Hooks.Path, Refused: map[int]string{}}
	for _, d := range res.Hooks.Diagnostics {
		h.Refused[d.Line] = d.Message
	}
	return h, nil
}

// HooksReload runs: agtermctl hooks reload --json.
func (a Agterm) HooksReload(ctx context.Context) error {
	if _, err := a.call(ctx, "hooks reload", "hooks", "reload", "--json"); err != nil {
		return fmt.Errorf("agterm: %w", err)
	}
	return nil
}

// noRun is an agterm run id that names no run.
const noRun = "00000000-0000-0000-0000-000000000000"

// KnowsEvent says whether agterm knows event kind. It asks for that kind's
// events after one of a run that never was: a kind agterm knows is answered
// "event run changed" at once, an unknown one "invalid event kind" (verified
// live). Any other answer reads as unknown.
func (a Agterm) KnowsEvent(ctx context.Context, kind string) bool {
	_, err := a.Run(ctx, a.Bin, "events", "--json", "--kind", kind, "--run", noRun, "--after", "0")
	return err != nil && strings.Contains(err.Error(), "event run changed")
}
