package launch

import (
	"context"
	"fmt"

	"github.com/IZZY-Agency/quartermaster/internal/plain"
)

// Notify posts a desktop notification of agterm: agtermctl notify BODY
// --title TITLE [--socket SOCKET] --json. socket is the control socket of
// the agterm to reach ("" for agtermctl's own choice: it does not read the
// environment's). body must not start with a dash.
func (a Agterm) Notify(ctx context.Context, socket, title, body string) error {
	args := []string{"notify", plain.Line(body), "--title", plain.Line(title)}
	if socket != "" {
		args = append(args, "--socket", socket)
	}
	if _, err := a.call(ctx, "notify", append(args, "--json")...); err != nil {
		return fmt.Errorf("agterm: %w", err)
	}
	return nil
}
