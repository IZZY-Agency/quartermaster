package up

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
)

// UsableRecords is recs with the fields of worker Records that qm up cannot
// use safely cleared, and a note for each Record changed. A field is
// unusable when it is a name that is not a session name
// (spawn.ValidName), a branch that is not a plain branch name
// (validBranch), or a folder (cwd, project) that is not an absolute path.
// The names become file names and the branches and folders git and gh
// arguments. Clearing a name or cwd means the worker is never started; a
// cleared branch or project folder only skips the checks that read it.
// The Record itself stays, so its session still counts as a worker of its
// project (ownership, replacements) and the rest of the project goes on.
// Records of other kinds are kept as they are.
func UsableRecords(recs []route.Record) ([]route.Record, []string) {
	out := make([]route.Record, len(recs))
	copy(out, recs)
	var notes []string
	for i := range out {
		r := &out[i]
		if r.Kind != "worker" {
			continue
		}
		var why []string
		start := true
		if !spawn.ValidName(r.Name) {
			why = append(why, fmt.Sprintf("name %q is not a usable session name", r.Name))
			start = false
		}
		if r.Cwd != "" && !filepath.IsAbs(r.Cwd) {
			why = append(why, fmt.Sprintf("folder %q is not an absolute path", r.Cwd))
			r.Cwd, start = "", false
		}
		if r.Branch != "" && !validBranch(r.Branch) {
			why = append(why, fmt.Sprintf("branch %q is not a usable branch name", r.Branch))
			r.Branch = ""
		}
		if r.Project != "" && !filepath.IsAbs(r.Project) {
			why = append(why, fmt.Sprintf("project folder %q is not an absolute path", r.Project))
			r.Project = ""
		}
		if len(why) == 0 {
			continue
		}
		what := "its finished check skips it"
		if !start {
			r.Name = ""
			what = "the worker is not started"
		}
		notes = append(notes, fmt.Sprintf("decision log: worker Record of session %q: %s: %s", r.ClaudeSession, strings.Join(why, "; "), what))
	}
	return out, notes
}

// validBranch says whether b is a branch name qm passes to git and gh: a
// name git check-ref-format --branch takes (no control characters, and
// UTF-8 letters allowed), that does not start with - (so no command reads
// it as a flag).
func validBranch(b string) bool {
	if b == "" || b == "@" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") ||
		strings.HasSuffix(b, ".") || strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") {
		return false
	}
	for i := 0; i < len(b); i++ {
		if c := b[i]; c <= ' ' || c == 0x7f || strings.IndexByte("~^:?*[\\", c) >= 0 {
			return false
		}
	}
	for part := range strings.SplitSeq(b, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// isDir says whether dir is an absolute path to an existing folder.
func isDir(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// fallbackName is the session name of an adopted session whose own name is
// not usable: w- and the first eight letters or digits of its session id
// (a session id's first block), or of a hash of the id when it has none.
func fallbackName(sessionID string) string {
	var b strings.Builder
	for _, c := range sessionID {
		if b.Len() == 8 {
			break
		}
		if c < 0x80 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		sum := sha256.Sum256([]byte(sessionID))
		b.WriteString(hex.EncodeToString(sum[:4]))
	}
	return "w-" + b.String()
}
