package main

import (
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/config"
)

func TestHandoffLine(t *testing.T) {
	cfg := config.Defaults()
	if got := handoffLine(cfg); got != "85% of the window" {
		t.Errorf("default: %q", got)
	}
	n := 70
	cfg.Worker.HandoffPercent = &n
	if got := handoffLine(cfg); got != "70% of the window" {
		t.Errorf("70: %q", got)
	}
	n = 150
	if got := handoffLine(cfg); !strings.HasPrefix(got, "85% of the window (") || !strings.Contains(got, "worker.handoff_percent is 150") {
		t.Errorf("out of range: %q", got)
	}
}
