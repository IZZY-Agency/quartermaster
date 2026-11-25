package up

import (
	"fmt"

	"github.com/IZZY-Agency/quartermaster/internal/claudecode"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// startingNow are the sessions a running qm spawn worker is starting
// (spawn.ListStarting), with every other id their processes hold: never
// adopted until that qm has logged them, tied to a project or not. One whose
// qm stopped in the middle is held by its reserved Record alone.
func startingNow(live map[string]claudecode.LiveSession) (map[string]bool, error) {
	st, err := spawn.ListStarting()
	if err != nil {
		return nil, fmt.Errorf("%w; nothing adopted", err)
	}
	held := map[string]bool{}
	for _, s := range st {
		if s.Running {
			held[s.SessionID] = true
		}
	}
	return withAliases(held, live), nil
}
