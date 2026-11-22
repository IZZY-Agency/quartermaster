package main

import (
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
// when there is no claude ancestor or its session is not known.
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
	live, err := claudecode.Live(dir, procs)
	if err != nil {
		return nil, err
	}
	var ids []string
	for id, l := range live {
		if l.PID == anc.PID {
			ids = append(ids, id)
		}
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
