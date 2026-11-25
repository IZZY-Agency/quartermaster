package main

import (
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// readUpRecords reads the whole decision log for qm up and its subcommands,
// with the Records of the workers qm spawn worker is starting:
// the unusable fields of worker Records are cleared by up.UsableRecords,
// each such Record noted on out, and the rest of the project goes on.
func readUpRecords(out io.Writer) ([]route.Record, error) {
	// Listed before the log is read (spawn.AddStarting).
	starting, err := spawn.ListStarting()
	if err != nil {
		return nil, err
	}
	recs, err := declog.Read(time.Time{})
	if err != nil {
		return nil, err
	}
	recs = spawn.AddStarting(recs, starting)
	recs, notes := up.UsableRecords(recs)
	printNotes(out, notes)
	return recs, nil
}

// recoverSpawns logs the Records of the workers a qm spawn worker that
// stopped in the middle left reserved, before qm up in project id reads the
// log. What could not be recovered is said on stderr and fails the run (ok
// false) but does not stop it. err stops it: a worker of the project may be
// running, and what runs could not be read.
func recoverSpawns(id string, stdout, stderr io.Writer) (ok bool, err error) {
	starting, err := spawn.ListStarting()
	if err != nil {
		return false, err
	}
	for _, s := range starting {
		if s.Running || !slices.ContainsFunc(s.Records, func(r route.Record) bool { return r.UpProject == id }) {
			continue
		}
		if _, err := upLive(); err != nil {
			return false, fmt.Errorf("%w; nothing started (an earlier qm spawn worker stopped while starting a session, which may be running)", err)
		}
		break
	}
	notes, err := spawn.RecoverStarting(upLog)
	printNotes(stdout, notes)
	if err != nil {
		printErr(stderr, "qm up", err)
	}
	return err == nil, nil
}

// spawnsResult is a run's result err, or errQuiet when what a stopped spawn
// left could not be recovered (already said on stderr).
func spawnsResult(err error, ok bool) error {
	if err == nil && !ok {
		return errQuiet
	}
	return err
}

// doneOf is cands without the sessions of worker name that never ran (not
// live, no transcript: a launch that failed) when another of the name did:
// qm up done means that one. never are the ones of them whose launch nothing
// confirmed, which end with it: a later qm up could take one for the worker. A
// confirmed launch may be about to run, and is in neither. While qm spawn
// worker starts one of the name it is an error, since which one is meant is
// not clear; before are the ones it was starting before cands were read, which
// may not run yet.
func doneOf(name string, cands []up.Candidate, before []spawn.Starting) (of, never []up.Candidate, err error) {
	starting, err := spawn.ListStarting()
	if err != nil {
		return nil, nil, err
	}
	starting = append(starting, before...)
	named := func(c up.Candidate) bool { return c.Role == up.RoleWorker && c.Name == name }
	for _, s := range starting {
		if s.Running && slices.ContainsFunc(cands, func(c up.Candidate) bool { return named(c) && c.SessionID == s.SessionID }) {
			return nil, nil, fmt.Errorf("qm spawn worker is starting %s: run it again in a moment", name)
		}
	}
	ran := false
	for _, c := range cands {
		switch {
		case !named(c):
		case c.Ran():
			ran = true
		case c.Record != nil && spawn.Unconfirmed(c.Record):
			never = append(never, c)
		}
	}
	if !ran {
		return cands, never, nil
	}
	return slices.DeleteFunc(slices.Clone(cands), func(c up.Candidate) bool { return named(c) && !c.Ran() }), never, nil
}
