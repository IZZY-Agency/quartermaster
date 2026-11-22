package setup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
)

// Undo is qm init --undo: it puts the status line back (asking when it was
// edited since init), removes the files init created and its record, and
// prints the /plugin uninstall command. The decision log stays.
func Undo(_ context.Context, s Streams, o Options) error {
	recPath, err := recordPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(recPath); errors.Is(err, fs.ErrNotExist) {
		say(s.Out, "Nothing to undo: %s not found\n", recPath)
		return nil
	}
	rec, err := readRecord()
	if err != nil {
		return err
	}
	p := prompter{in: bufio.NewReader(s.In), out: s.Out, yes: o.Yes, term: terminalOf(s.In)}
	// The settings file init worked on, wherever Claude Code's folder is now.
	settingsPath := rec.SettingsPath
	if settingsPath == "" {
		if settingsPath, err = settingsFile(o); err != nil {
			return err
		}
	}

	if rec.StatusLineAfter != "" {
		if err := restoreStatusLine(p, settingsPath, rec); err != nil {
			return err
		}
	}

	cfgPath, _ := config.Path()
	for _, path := range slices.Backward(rec.Created) {
		// Every created file but the config is a Claude Code settings file
		// (an earlier init may have run with another CLAUDE_CONFIG_DIR):
		// one that holds other settings now is kept.
		if path != cfgPath && !emptySettings(path) {
			say(s.Out, "Kept: %s (init created it; it now holds other settings)\n", path)
			continue
		}
		if rec.ConfigSHA256 != "" && editedSince(path, rec.ConfigSHA256) &&
			!p.askOverEdit(path+" was edited since qm init. Remove it anyway?") {
			say(s.Out, "Kept: %s\n", path)
			continue
		}
		if err := os.Remove(path); err == nil {
			say(s.Out, "Removed: %s\n", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(recPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(recPath)) // only when init left it empty

	say(s.Out, "\nInside Claude Code, run:\n  %s\n", pluginUninstall)
	if log, err := declog.Path(); err == nil {
		say(s.Out, "The decision log is kept: %s\n", log)
	}
	return nil
}

// restoreStatusLine puts back the status line init replaced. When the
// settings file is still exactly what init made of the backup, the backup is
// copied back byte for byte; when only the command is still init's, the
// command is put back; otherwise the user is shown both and asked.
func restoreStatusLine(p prompter, path string, rec Record) error {
	settings, existed, err := readSettings(path)
	if err != nil {
		return err
	}
	if !existed {
		say(p.out, "Status line: %s is gone; nothing to restore\n", path)
		return nil
	}
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	var backup []byte
	if rec.SettingsBackup != "" {
		if backup, err = os.ReadFile(rec.SettingsBackup); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if backup != nil {
		if wrapped, _, changed := WrapStatusLine(backup); changed && bytes.Equal(wrapped, settings) {
			if err := writeFileAtomic(path, backup, mode); err != nil {
				return err
			}
			say(p.out, "Status line: restored %s from %s\n", path, rec.SettingsBackup)
			return removeBackup(p, rec.SettingsBackup)
		}
	}

	before := "(none)"
	if rec.StatusLineBefore != nil {
		before = *rec.StatusLineBefore
	}
	cur, ok := statusLineCommand(settings)
	if !ok || cur != rec.StatusLineAfter {
		now := "(none)"
		if ok {
			now = cur
		}
		say(p.out, "Status line was edited since qm init:\n  now:          %s\n  init wrote:   %s\n  before init:  %s\n", now, rec.StatusLineAfter, before)
		if !p.askOverEdit("Put back the status line from before init?") {
			say(p.out, "Status line: kept your edit; before init it was: %s\n", before)
			if rec.SettingsBackup != "" {
				say(p.out, "Backup kept: %s\n", rec.SettingsBackup)
			}
			return nil
		}
	}
	updated, err := setStatusLine(settings, rec.StatusLineBefore)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, updated, mode); err != nil {
		return err
	}
	say(p.out, "Status line: put back to %s\n", before)
	if rec.SettingsBackup != "" {
		return removeBackup(p, rec.SettingsBackup)
	}
	return nil
}

func removeBackup(p prompter, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	say(p.out, "Removed: %s\n", path)
	return nil
}

// editedSince reports whether the file at path exists and no longer has the
// hash init recorded. Only the config has one; any other path is not edited.
func editedSince(path, hash string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	cfg, err := config.Path()
	return err == nil && path == cfg && sha(b) != hash
}

// emptySettings reports whether the settings file is missing or holds an
// object with no members.
func emptySettings(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	o, err := topObject(b)
	return err == nil && len(o.members) == 0
}
