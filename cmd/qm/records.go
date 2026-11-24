package main

import (
	"io"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/up"
)

// readUpRecords reads the whole decision log for qm up and its subcommands:
// the unusable fields of worker Records are cleared by up.UsableRecords,
// each such Record noted on out, and the rest of the project goes on.
func readUpRecords(out io.Writer) ([]route.Record, error) {
	recs, err := declog.Read(time.Time{})
	if err != nil {
		return nil, err
	}
	recs, notes := up.UsableRecords(recs)
	printNotes(out, notes)
	return recs, nil
}
