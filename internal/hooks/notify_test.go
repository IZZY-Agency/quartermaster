package hooks

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
)

func TestNotifyPipeHeldByChildIsBounded(t *testing.T) {
	old := notifyWaitDelay
	notifyWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { notifyWaitDelay = old })
	cfg := config.Defaults()
	// The notifier exits at once but leaves a child holding its output.
	cfg.Notify = []string{"sh", "-c", "sleep 30 & exit 0"}
	start := time.Now()
	_ = NotifyContext(context.Background(), cfg, "t", "x")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("notify waited %v for the child's pipe", d)
	}
}

func TestNotifyCancelled(t *testing.T) {
	cfg := config.Defaults()
	cfg.Notify = []string{"sleep", "30"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := NotifyContext(ctx, cfg, "t", "x"); err == nil {
		t.Fatal("a cancelled notifier must fail")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

func TestNotifyOutputBounded(t *testing.T) {
	cfg := config.Defaults()
	cfg.Notify = []string{"sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x; exit 3"}
	err := NotifyContext(context.Background(), cfg, "t", "x")
	if err == nil || len(err.Error()) > notifyOutputMax+200 || !strings.Contains(err.Error(), "xxx") {
		t.Fatalf("err %d bytes: %.80v", len(err.Error()), err)
	}
}
