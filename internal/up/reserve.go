package up

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// takeCoordinator makes the coordinator step s, whose session runs under sid,
// the project's coordinator. When Gather fell back to a past coordinator (the
// current one never ran), that one is current again and the never-run id goes
// to the past ones, so it stays held: its launch line may still be run. A
// fresh start (sid is not s's session) moves s's session to the past ones.
func (p *Project) takeCoordinator(s Step, sid string, started Session) {
	past := slices.Clone(p.PastCoordinators)
	if s.SessionID != p.Coordinator.ID {
		past = slices.DeleteFunc(past, func(id string) bool { return id == s.SessionID })
		if id := p.Coordinator.ID; id != "" && !slices.Contains(past, id) {
			past = append(past, id)
		}
		p.Coordinator = Session{ID: s.SessionID, Name: s.Name, Started: s.Started}
	}
	if sid != s.SessionID {
		past = append(past, s.SessionID)
		p.Coordinator = started
	}
	p.PastCoordinators = past
}

// reserve saves the new session id sid of fresh step s before it is started,
// as a first run saves its coordinator's, so there is no moment at which the
// session runs and no saved state holds it: a coordinator's as the project's
// coordinator, a worker's as a terminal session with no id (what holds it
// against an adoption) with the Record to log (Starting). withdraw undoes it,
// its default model included, in p and on disk, when the launch failed. A failed save leaves p as it was.
func (p *Project) reserve(s Step, sid string, started Session, rec *route.Record) (withdraw func() error, err error) {
	coord, past, starting := p.Coordinator, p.PastCoordinators, maps.Clone(p.Starting)
	undo := func() {
		p.Coordinator, p.PastCoordinators, p.Starting = coord, past, starting
		if rec != nil {
			delete(p.Sessions, sid)
		}
		delete(p.DefaultModels, sid)
	}
	if rec == nil {
		p.takeCoordinator(s, sid, started)
	} else {
		if p.Starting == nil {
			p.Starting = map[string]route.Record{}
		}
		p.Starting[sid] = *rec
		p.Sessions[sid] = launch.SessionRef{Name: s.Name}
	}
	if err := Save(*p); err != nil {
		undo()
		return nil, err
	}
	return func() error {
		undo()
		return Save(*p)
	}, nil
}

// unreserve ends worker sid's reservation: its Record is logged. A terminal
// session recorded meanwhile stays.
func (p *Project) unreserve(sid string) {
	delete(p.Starting, sid)
	if ref, ok := p.Sessions[sid]; ok && ref.ID == "" {
		delete(p.Sessions, sid)
	}
}

// withStarting is recs with the reserved Records (Project.Starting) the log
// does not have yet: a session of a qm up that stopped is planned, marked
// done and closed as if its Record was logged.
func (p *Project) withStarting(recs []route.Record) []route.Record {
	for _, sid := range slices.Sorted(maps.Keys(p.Starting)) {
		if !loggedWorker(recs, sid) {
			usable, _ := UsableRecords([]route.Record{p.Starting[sid]})
			recs = append(slices.Clone(recs), usable...)
		}
	}
	return recs
}

func loggedWorker(recs []route.Record, sid string) bool {
	return slices.ContainsFunc(recs, func(r route.Record) bool { return r.Kind == "worker" && r.ClaudeSession == sid })
}

// RecoverStarts ends the reservations (Project.Starting) a qm up left that
// stopped between a fresh worker's reservation and its Record, or could not
// log that Record: the Record is logged and added to recs, so Gather leaves
// the session when it runs and restores the one it replaces when it never
// did, as after a run without a launcher. A Record the log has already is not
// logged again. One that cannot be logged stays reserved; err says so, or
// that p was not saved.
func RecoverStarts(p *Project, recs []route.Record, d Deps) (_ []route.Record, notes []string, err error) {
	if len(p.Starting) == 0 {
		return recs, nil, nil
	}
	full := p.withStarting(recs)
	var errs []error
	for _, sid := range slices.Sorted(maps.Keys(p.Starting)) {
		r := p.Starting[sid]
		if !loggedWorker(recs, sid) && d.Log != nil {
			if err := d.Log(r); err != nil {
				errs = append(errs, fmt.Errorf("worker %s: its Record (session %s) was not logged: %w", r.Name, prefix(sid), err))
				continue
			}
			notes = append(notes, fmt.Sprintf("worker %s: an earlier qm up stopped while starting session %s: its Record is logged now", r.Name, prefix(sid)))
		}
		p.unreserve(sid)
	}
	if err := Save(*p); err != nil {
		errs = append(errs, fmt.Errorf("the project was not saved: %w", err))
	}
	return full, notes, errors.Join(errs...)
}
