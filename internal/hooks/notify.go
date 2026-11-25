package hooks

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/plain"
)

// notifyTimeout bounds one notifier run, so a stuck notifier cannot hold a
// hook past its deadline.
const notifyTimeout = 10 * time.Second

// notifyOutputMax bounds the notifier output kept for an error.
const notifyOutputMax = 2048

// notifyWaitDelay bounds the wait for the notifier's output to close after
// it exits or is killed: a child it left behind may hold the pipe.
var notifyWaitDelay = 2 * time.Second

// capped keeps the first max bytes written to it and drops the rest.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// runCommand runs argv and waits for it, bounded by ctx and notifyTimeout;
// tests replace it.
var runCommand = func(ctx context.Context, argv []string) error {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out := &capped{max: notifyOutputMax}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = notifyWaitDelay
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.buf.String()))
	}
	return nil
}

// Notify is NotifyContext without a caller's context.
func Notify(cfg config.Config, title, text string) error {
	return NotifyContext(context.Background(), cfg, title, text)
}

// NotifyContext runs the user's notifier, cfg.Notify, with one more
// argument: the message, "title: text" (or text alone when title is
// empty). An empty cfg.Notify means no notifier: nothing runs and it
// returns nil. ctx ending stops the notifier.
func NotifyContext(ctx context.Context, cfg config.Config, title, text string) error {
	if len(cfg.Notify) == 0 {
		return nil
	}
	msg := text
	if title != "" {
		msg = title + ": " + text
	}
	argv := append(append([]string{}, cfg.Notify...), plain.Line(msg))
	if err := runCommand(ctx, argv); err != nil {
		return fmt.Errorf("notify %s: %w", cfg.Notify[0], err)
	}
	return nil
}
