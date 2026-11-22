package claudecode

import "time"

// ProcStart is when p started, from ps's lstart in local time; false when
// it does not parse.
func ProcStart(p Proc) (time.Time, bool) {
	t, err := time.ParseInLocation(lstartLayout, collapse(p.Start), time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
