package main

import (
	"context"
	"errors"
	"slices"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/paths"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// callerSessions are the Claude session ids of the claude process qm runs
// under: the first claude ancestor of upSelfPID (CLAUDE_PID, else qm's
// parent), looked up in the live sessions. One process may hold two ids
// (the one its arguments name and the one its session file names, after
// /clear or an in-session /resume), so every one is returned, sorted. None
// when there is no claude ancestor or it has no session; an error when its
// session is not known because the session data could not be read.
func callerSessions() ([]string, error) {
	procs, err := upProcs()
	if err != nil {
		return nil, err
	}
	anc, ok := claudecode.Ancestor(procs, upSelfPID(procs))
	if !ok {
		return nil, nil
	}
	dir, err := paths.ClaudeDir()
	if err != nil {
		return nil, err
	}
	live, err := claudecode.LiveComplete(context.Background(), dir, procs)
	var inc *claudecode.IncompleteError
	if err != nil && !errors.As(err, &inc) {
		return nil, err
	}
	var ids []string
	for id, l := range live {
		if l.PID == anc.PID {
			ids = append(ids, id)
		}
	}
	if inc != nil && len(ids) == 0 && slices.ContainsFunc(inc.Unread, func(u claudecode.Unread) bool {
		return u.PID == anc.PID || u.PID == 0 // the caller's, or no known process's
	}) {
		return nil, inc
	}
	slices.Sort(ids)
	return ids, nil
}

// projectHolds says whether session id is one of p's: its coordinator, an
// adopted session, or a session a worker Record ties to p.
func projectHolds(p *up.Project, recs []route.Record, id string) bool {
	if id == "" {
		return false
	}
	if id == p.Coordinator.ID {
		return true
	}
	if _, ok := p.Adopted[id]; ok {
		return true
	}
	for i := range recs {
		if r := &recs[i]; r.Kind == "worker" && r.ClaudeSession == id && p.Tied(r) {
			return true
		}
	}
	return false
}
