package hooks

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
)

// notifyTimeout bounds one notifier run, so a stuck notifier cannot hold a
// hook past its deadline.
const notifyTimeout = 10 * time.Second

// runCommand runs argv and waits for it; tests replace it.
var runCommand = func(argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Notify runs the user's notifier, cfg.Notify, with one more argument: the
// message, "title: text" (or text alone when title is empty). An empty
// cfg.Notify means no notifier: nothing runs and Notify returns nil.
func Notify(cfg config.Config, title, text string) error {
	if len(cfg.Notify) == 0 {
		return nil
	}
	msg := text
	if title != "" {
		msg = title + ": " + text
	}
	argv := append(append([]string{}, cfg.Notify...), msg)
	if err := runCommand(argv); err != nil {
		return fmt.Errorf("notify %s: %w", cfg.Notify[0], err)
	}
	return nil
}
