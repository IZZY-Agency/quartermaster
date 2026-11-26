package up

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/launch"
)

// Proof is what qm finish proved before it changes anything: the worker's
// branch has a merged pull request whose head is the branch's tip.
type Proof struct {
	Branch string
	// Head is the commit that is both the branch's tip and the merged pull
	// request's head.
	Head   string
	Number int
	URL    string
	// Repo is the pull request's head repository, "owner/name", on Host
	// (the host of the pull request's address).
	Repo, Host string
	// RepoDir is the main checkout of the worker's repository, where every
	// later git and gh call runs.
	RepoDir string
	// Default is the repository's default branch.
	Default string
}

// String is the proof as qm finish prints it.
func (p Proof) String() string {
	return fmt.Sprintf("pull request #%d (%s) is merged, and branch %s's tip %s is its head", p.Number, p.URL, p.Branch, short(p.Head))
}

// ghPR is one pull request as gh pr list prints it for ProveMerged.
type ghPR struct {
	Number              int                    `json:"number"`
	State               string                 `json:"state"`
	HeadRefOid          string                 `json:"headRefOid"`
	IsCrossRepository   bool                   `json:"isCrossRepository"`
	CreatedAt           time.Time              `json:"createdAt"`
	URL                 string                 `json:"url"`
	HeadRepository      struct{ Name string }  `json:"headRepository"`
	HeadRepositoryOwner struct{ Login string } `json:"headRepositoryOwner"`
}

const ghFinishFields = "number,state,headRefOid,isCrossRepository,createdAt,url,headRepository,headRepositoryOwner"

// ProveMerged proves that worker c is finished: its branch has a merged pull
// request, from the branch's own repository, whose head is the branch's tip,
// and no pull request of the branch is still open. The branch is the one
// checked out in c's folder now, and the repository that folder's. Only when
// the folder is gone are the Record's branch and the repository of one of
// fallbacks (the Record's project folder, then the project's root) taken,
// and that one only when it held the folder: the folder was in its main
// checkout, or it still lists the folder as a worktree. gh runs in that
// repository's main checkout (ExecDir keeps GH_REPO out). The error says
// why there is no proof.
func ProveMerged(ctx context.Context, run DirRunner, c Candidate, fallbacks ...string) (Proof, error) {
	var pr Proof
	b := c.Branch
	if !filepath.IsAbs(c.Cwd) {
		return pr, fmt.Errorf("the worker's folder %q is not an absolute path", c.Cwd)
	}
	if isDir(c.Cwd) {
		wts, err := worktrees(ctx, run, c.Cwd)
		if err != nil {
			return pr, fmt.Errorf("the worktrees of %s were not listed: %w", c.Cwd, err)
		}
		if len(wts) == 0 || wts[0].Bare {
			return pr, fmt.Errorf("the repository of %s has no main checkout", c.Cwd)
		}
		pr.RepoDir = wts[0].Path
		out, err := run(ctx, c.Cwd, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return pr, fmt.Errorf("the worker's folder %s is on no branch", c.Cwd)
		}
		b = gitLine(out)
	} else {
		if c.Record != nil {
			fallbacks = append([]string{c.Record.Project}, fallbacks...)
		}
		for _, d := range fallbacks {
			if !isDir(d) {
				continue
			}
			wts, err := worktrees(ctx, run, d)
			if err != nil || len(wts) == 0 || wts[0].Bare {
				continue
			}
			if heldFolder(wts, c.Cwd) {
				pr.RepoDir = wts[0].Path
				break
			}
		}
		if pr.RepoDir == "" {
			return pr, fmt.Errorf("the worker's folder %s is gone, and no repository of the project held it", c.Cwd)
		}
	}
	if b == "" || b == "HEAD" || !validBranch(b) {
		return pr, fmt.Errorf("the worker is on no branch qm can name (%q)", b)
	}
	pr.Default = defaultBranch(ctx, run, pr.RepoDir)
	if b == pr.Default || pr.Default == "" && (b == "main" || b == "master") {
		return pr, fmt.Errorf("branch %s is the default branch", b)
	}
	if !validBranch(pr.Default) {
		return pr, errors.New("the repository's default branch was not found")
	}
	tip, err := run(ctx, pr.RepoDir, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+b+"^{commit}")
	if err != nil {
		return pr, fmt.Errorf("there is no local branch %s: nothing shows which commit was merged", b)
	}
	pr.Branch, pr.Head = b, gitLine(tip)
	out, err := run(ctx, pr.RepoDir, "gh", "pr", "list", "--head", b, "--state", "all", "--json", ghFinishFields, "--limit", ghPRLimit)
	if err != nil {
		return pr, fmt.Errorf("the pull requests of branch %s were not read: %w", b, err)
	}
	var prs []ghPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return pr, fmt.Errorf("the pull requests of branch %s were not read: %w", b, err)
	}
	prs = slices.DeleteFunc(prs, func(p ghPR) bool { return p.IsCrossRepository })
	if len(prs) == 0 {
		return pr, fmt.Errorf("branch %s has no pull request", b)
	}
	for _, p := range prs {
		if p.State == "OPEN" {
			return pr, fmt.Errorf("pull request #%d of branch %s is still open", p.Number, b)
		}
	}
	merged := slices.IndexFunc(prs, func(p ghPR) bool { return p.State == "MERGED" && p.HeadRefOid == pr.Head })
	if merged < 0 {
		newest := slices.MaxFunc(prs, func(a, b ghPR) int { return a.CreatedAt.Compare(b.CreatedAt) })
		if newest.State != "MERGED" {
			return pr, fmt.Errorf("pull request #%d of branch %s is %s, not merged", newest.Number, b, strings.ToLower(newest.State))
		}
		return pr, fmt.Errorf("branch %s is at %s, but merged pull request #%d's head is %s: the branch moved", b, short(pr.Head), newest.Number, short(newest.HeadRefOid))
	}
	p := prs[merged]
	if p.HeadRepositoryOwner.Login == "" || p.HeadRepository.Name == "" {
		return pr, fmt.Errorf("gh did not name the repository of pull request #%d", p.Number)
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Hostname() == "" {
		return pr, fmt.Errorf("gh did not give the address of pull request #%d", p.Number)
	}
	pr.Number, pr.URL, pr.Host, pr.Repo = p.Number, p.URL, u.Hostname(), p.HeadRepositoryOwner.Login+"/"+p.HeadRepository.Name
	return pr, nil
}

// worktree is one entry of git worktree list.
type worktree struct {
	Path, Head, Branch string
	Bare, Prunable     bool
}

// heldFolder says whether the repository of worktrees wts held folder dir,
// which is gone: dir was inside its main checkout, or it still lists dir.
func heldFolder(wts []worktree, dir string) bool {
	main := realPath(wts[0].Path)
	if d := filepath.Clean(dir); strings.HasPrefix(d, main+string(filepath.Separator)) {
		return true
	}
	return slices.ContainsFunc(wts, func(w worktree) bool {
		return filepath.Clean(w.Path) == filepath.Clean(dir) || realPath(w.Path) == filepath.Clean(dir)
	})
}

// worktrees lists the worktrees of the repository at dir, the main checkout
// first (git worktree list --porcelain -z, so no path splits them).
func worktrees(ctx context.Context, run DirRunner, dir string) ([]worktree, error) {
	out, err := run(ctx, dir, "git", "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var wts []worktree
	var cur *worktree
	for f := range bytes.SplitSeq(out, []byte{0}) {
		k, v, _ := strings.Cut(string(f), " ")
		switch k {
		case "worktree":
			wts = append(wts, worktree{Path: v})
			cur = &wts[len(wts)-1]
		case "HEAD":
			if cur != nil {
				cur.Head = v
			}
		case "branch":
			if cur != nil {
				cur.Branch = v
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		case "prunable":
			if cur != nil {
				cur.Prunable = true
			}
		}
	}
	return wts, nil
}

// ProcCwd is a running process and its current folder. Raw is the folder
// as lsof printed it when that had escapes, which Cwd has decoded.
type ProcCwd struct {
	PID int
	Cmd string
	Cwd string
	Raw string
}

// Finisher runs qm finish's steps after the proof. Each step changes nothing
// when DryRun is set, and returns its one line: what it did or would do, or
// why it left something as it was.
type Finisher struct {
	Run DirRunner
	// Procs lists the running processes' current folders.
	Procs func(ctx context.Context) ([]ProcCwd, error)
	// Sleep waits between checks of a worktree's users (time.Sleep).
	Sleep  func(time.Duration)
	DryRun bool
}

// usersWait is how long RemoveWorktree waits for the processes in a worktree
// to end, after the worker's terminal session was closed.
const (
	usersWait  = 5 * time.Second
	usersCheck = 500 * time.Millisecond
)

// StepResult is the outcome of one step after the proof: its line, and
// whether it failed (an action or a read went wrong), as against leaving
// something on purpose, which the line says.
type StepResult struct {
	Line   string
	Failed bool
}

func leftf(format string, args ...any) StepResult {
	return StepResult{Line: fmt.Sprintf(format, args...)}
}

func failedf(format string, args ...any) StepResult {
	return StepResult{Line: fmt.Sprintf(format, args...), Failed: true}
}

// RemoveWorktree removes the linked worktree that worker folder cwd is in,
// with git worktree remove (never --force), when its checked out branch and
// commit are the proof's, it has no uncommitted or untracked files, no other
// worktree or repository is inside it (registered, or in an ignored folder),
// and no running process has its current folder in it. Its ignored files go
// with it, and the line names them. waitUsers: the worker's terminal session
// was just closed, so its processes may take a moment to end; their end is
// waited for. A folder that is gone while git still lists it as a worktree
// of the branch loses that entry. removed is the worktree's folder when it
// removed it (or would, on a dry run), else "".
func (f Finisher) RemoveWorktree(ctx context.Context, pr Proof, cwd string, waitUsers bool) (res StepResult, removed string) {
	if !filepath.IsAbs(cwd) {
		return leftf("worktree: left: the worker's folder %q is not an absolute path", cwd), ""
	}
	wts, err := worktrees(ctx, f.Run, pr.RepoDir)
	if err != nil {
		return failedf("worktree %s: left: the worktrees were not listed: %v", cwd, err), ""
	}
	if !isDir(cwd) {
		return f.removeGone(ctx, pr, cwd, wts)
	}
	top, err := f.Run(ctx, cwd, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return failedf("worktree %s: left: its top folder was not read: %v", cwd, err), ""
	}
	path := gitLine(top)
	i := slices.IndexFunc(wts, func(w worktree) bool { return realPath(w.Path) == realPath(path) })
	switch {
	case i < 0:
		return leftf("worktree %s: left: it is not a worktree of %s", path, pr.RepoDir), ""
	case i == 0:
		return leftf("worktree %s: left: it is the main checkout, never removed", path), ""
	case wts[i].Branch != "refs/heads/"+pr.Branch:
		return leftf("worktree %s: left: it is not on branch %s now", path, pr.Branch), ""
	case wts[i].Head != pr.Head:
		return leftf("worktree %s: left: it is at %s, not the merged head %s", path, short(wts[i].Head), short(pr.Head)), ""
	}
	for j, w := range wts {
		if j != i && inside(realPath(w.Path), realPath(path)) {
			return leftf("worktree %s: left: worktree %s is inside it", path, w.Path), ""
		}
	}
	status, err := f.Run(ctx, path, "git", "status", "--porcelain", "-z", "--no-renames", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none")
	if err != nil {
		return failedf("worktree %s: left: its status was not read: %v", path, err), ""
	}
	changed, ignored := 0, []string(nil)
	for e := range strings.SplitSeq(string(status), "\x00") {
		switch {
		case e == "":
		case strings.HasPrefix(e, "!! "):
			ignored = append(ignored, e[3:])
		default:
			changed++
		}
	}
	if changed > 0 {
		return leftf("worktree %s: left: %d uncommitted or untracked file(s)", path, changed), ""
	}
	for _, ig := range ignored {
		if !strings.HasSuffix(ig, "/") {
			continue
		}
		repo, err := nestedRepo(ctx, filepath.Join(path, ig))
		if err != nil {
			return failedf("worktree %s: left: its ignored folder %s was not read: %v", path, ig, err), ""
		}
		if repo != "" {
			return leftf("worktree %s: left: its ignored folder %s holds another repository or worktree (%s)", path, ig, repo), ""
		}
	}
	users, err := f.users(ctx, path)
	for wait := usersWait; err == nil && len(users) > 0 && waitUsers && !f.DryRun && wait > 0; wait -= usersCheck {
		f.Sleep(usersCheck)
		users, err = f.users(ctx, path)
	}
	if err != nil {
		return failedf("worktree %s: left: the running processes' folders were not read: %v", path, err), ""
	}
	if len(users) > 0 {
		var who []string
		for _, u := range users {
			who = append(who, fmt.Sprintf("pid %d (%s)", u.PID, u.Cmd))
		}
		return leftf("worktree %s: left: it is the current folder of %s", path, strings.Join(who, ", ")), ""
	}
	also := ""
	if len(ignored) > 0 {
		also = fmt.Sprintf("; its %d ignored file(s) or folder(s) go with it: %s", len(ignored), sample(ignored))
	}
	if f.DryRun {
		return leftf("worktree %s: would remove (git worktree remove): on %s at the merged head, clean, no process in it%s", path, pr.Branch, also), path
	}
	if _, err := f.Run(ctx, pr.RepoDir, "git", "worktree", "remove", path); err != nil {
		return failedf("worktree %s: left: git worktree remove failed: %v", path, err), ""
	}
	return leftf("worktree %s: removed%s", path, also), path
}

// removeGone is RemoveWorktree for a worker folder cwd that is gone: git's
// entry for it, when git still lists it on the proof's branch, is removed.
func (f Finisher) removeGone(ctx context.Context, pr Proof, cwd string, wts []worktree) (StepResult, string) {
	i := slices.IndexFunc(wts, func(w worktree) bool {
		return filepath.Clean(w.Path) == filepath.Clean(cwd) || realPath(w.Path) == realGone(cwd)
	})
	if i <= 0 || !wts[i].Prunable {
		return leftf("worktree %s: already gone", cwd), ""
	}
	path := wts[i].Path
	if wts[i].Branch != "refs/heads/"+pr.Branch {
		return leftf("worktree %s: gone, but git lists it on another branch: its entry is left", path), ""
	}
	if f.DryRun {
		return leftf("worktree %s: gone; would remove git's entry for it (git worktree remove)", path), path
	}
	if _, err := f.Run(ctx, pr.RepoDir, "git", "worktree", "remove", path); err != nil {
		return failedf("worktree %s: gone, but git's entry for it was not removed: %v", path, err), ""
	}
	return leftf("worktree %s: gone; git's entry for it removed", path), path
}

// inside says whether path is below dir.
func inside(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// realGone is realPath for a path that may be gone: its nearest existing
// folder resolved, the rest joined back.
func realGone(dir string) string {
	dir = filepath.Clean(dir)
	rest := ""
	for d := dir; ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(d) == d {
			return dir
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}

// nestedRepo is the first .git (folder or file) below dir, "" when there is
// none: a repository or worktree that removing dir would take with it.
func nestedRepo(ctx context.Context, dir string) (string, error) {
	found := ""
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Name() == ".git" {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

// sample is the first few of names, and how many more there are.
func sample(names []string) string {
	const shown = 5
	if len(names) <= shown {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
}

// users are the running processes whose current folder is path or in it.
// A folder lsof printed with escapes counts in both its forms.
func (f Finisher) users(ctx context.Context, path string) ([]ProcCwd, error) {
	procs, err := f.Procs(ctx)
	if err != nil {
		return nil, err
	}
	real := realPath(path)
	var in []ProcCwd
	for _, p := range procs {
		for _, cwd := range []string{p.Cwd, p.Raw} {
			if c := realPath(cwd); cwd != "" && (c == real || inside(c, real)) {
				in = append(in, p)
				break
			}
		}
	}
	return in, nil
}

// DeleteBranch deletes the local branch, only at the proof's head (git
// update-ref -d with the old value, so a branch that moves meanwhile is left)
// and only when no worktree has it checked out. removed is the worktree
// RemoveWorktree removed, or would on a dry run, which then does not count.
func (f Finisher) DeleteBranch(ctx context.Context, pr Proof, removed string) StepResult {
	ref := "refs/heads/" + pr.Branch
	tip, err := f.Run(ctx, pr.RepoDir, "git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return leftf("branch %s: already gone", pr.Branch)
	}
	if t := gitLine(tip); t != pr.Head {
		return leftf("branch %s: left: its tip is %s now, not the merged head %s", pr.Branch, short(t), short(pr.Head))
	}
	wts, err := worktrees(ctx, f.Run, pr.RepoDir)
	if err != nil {
		return failedf("branch %s: left: the worktrees were not listed: %v", pr.Branch, err)
	}
	for _, w := range wts {
		if w.Branch == ref && (removed == "" || realPath(w.Path) != realPath(removed)) {
			return leftf("branch %s: left: it is checked out in %s", pr.Branch, w.Path)
		}
	}
	if f.DryRun {
		return leftf("branch %s: would delete at %s (git update-ref -d)", pr.Branch, short(pr.Head))
	}
	if _, err := f.Run(ctx, pr.RepoDir, "git", "update-ref", "-d", ref, pr.Head); err != nil {
		return failedf("branch %s: left: %v", pr.Branch, err)
	}
	// Its settings (upstream, merge) go with it, as with git branch -d; a
	// branch with none has no section to remove.
	_, _ = f.Run(ctx, pr.RepoDir, "git", "config", "--remove-section", "branch."+pr.Branch)
	return leftf("branch %s: deleted (was %s)", pr.Branch, short(pr.Head))
}

// DeleteRemoteBranch deletes the branch in the pull request's head
// repository through the git remote that names that repository (its fetch
// and push addresses both), only at the proof's head: git push
// --force-with-lease, so the remote deletes it only while its tip there is
// still that head, and the remote-tracking branch goes with it.
func (f Finisher) DeleteRemoteBranch(ctx context.Context, pr Proof) StepResult {
	name := pr.Repo + " " + pr.Branch
	remote, err := f.remoteOf(ctx, pr)
	if err != nil {
		return failedf("remote branch %s: left: the remotes were not read: %v", name, err)
	}
	if remote == "" {
		return leftf("remote branch %s: left: no git remote of %s is %s/%s", name, pr.RepoDir, pr.Host, pr.Repo)
	}
	ref := "refs/heads/" + pr.Branch
	out, err := f.Run(ctx, pr.RepoDir, "git", "ls-remote", "--exit-code", remote, ref)
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ee.ExitCode() == 2 {
		return leftf("remote branch %s: already gone", name)
	}
	if err != nil {
		return failedf("remote branch %s: left: its tip was not read: %v", name, err)
	}
	tip := lsRemoteTip(out, ref)
	if tip == "" {
		return failedf("remote branch %s: left: its tip was not read", name)
	}
	if tip != pr.Head {
		return leftf("remote branch %s: left: its tip is %s, not the merged head %s", name, short(tip), short(pr.Head))
	}
	if f.DryRun {
		return leftf("remote branch %s: would delete at %s (git push --force-with-lease to %s)", name, short(pr.Head), remote)
	}
	if _, err := f.Run(ctx, pr.RepoDir, "git", "push", "--porcelain", "--force-with-lease="+ref+":"+pr.Head, remote, ":"+ref); err != nil {
		// The lease refuses a branch that moved after the read: read again
		// to tell that from a push that failed.
		out, lerr := f.Run(ctx, pr.RepoDir, "git", "ls-remote", remote, ref)
		if now := lsRemoteTip(out, ref); lerr == nil && now != pr.Head {
			return leftf("remote branch %s: left: it moved since its tip was read", name)
		}
		return failedf("remote branch %s: left: git push failed: %v", name, err)
	}
	return leftf("remote branch %s: deleted (was %s)", name, short(pr.Head))
}

// lsRemoteTip is the commit git ls-remote printed for ref, "" for none.
func lsRemoteTip(out []byte, ref string) string {
	for line := range strings.SplitSeq(string(out), "\n") {
		if sha, r, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "\t"); ok && r == ref {
			return sha
		}
	}
	return ""
}

// remoteOf is the git remote of the main checkout whose fetch and push
// addresses all name the pull request's host and repository; "" when none
// does.
func (f Finisher) remoteOf(ctx context.Context, pr Proof) (string, error) {
	out, err := f.Run(ctx, pr.RepoDir, "git", "remote")
	if err != nil {
		return "", err
	}
	for r := range strings.SplitSeq(string(out), "\n") {
		r = strings.TrimSpace(r)
		if r == "" || strings.HasPrefix(r, "-") {
			continue
		}
		var urls []string
		for _, key := range []string{"url", "pushurl"} {
			// Not "git remote get-url": it applies insteadOf, and the
			// address as configured is the one that names the repository.
			if o, err := f.Run(ctx, pr.RepoDir, "git", "config", "--get-all", "remote."+r+"."+key); err == nil {
				urls = append(urls, strings.Fields(string(o))...)
			}
		}
		if len(urls) > 0 && !slices.ContainsFunc(urls, func(u string) bool { return !namesRepo(u, pr.Host, pr.Repo) }) {
			return r, nil
		}
	}
	return "", nil
}

// namesRepo says whether git address u is of repository repo ("owner/name")
// on host: https://host/owner/name(.git), ssh://[user@]host[:port]/owner/name,
// or [user@]host:owner/name.
func namesRepo(u, host, repo string) bool {
	var h, p string
	if strings.Contains(u, "://") {
		parsed, err := url.Parse(u)
		if err != nil {
			return false
		}
		h, p = parsed.Hostname(), parsed.Path
	} else {
		before, after, ok := strings.Cut(u, ":")
		if !ok || strings.Contains(before, "/") {
			return false // a local path
		}
		if _, hh, at := strings.Cut(before, "@"); at {
			before = hh
		}
		h, p = before, after
	}
	p = strings.TrimSuffix(strings.TrimSuffix(strings.Trim(p, "/"), "/"), ".git")
	return h != "" && strings.EqualFold(h, host) && strings.EqualFold(p, repo)
}

// FastForwardMain fast-forwards the main checkout (git pull --ff-only) when
// it is on the default branch with no uncommitted changes to tracked files.
func (f Finisher) FastForwardMain(ctx context.Context, pr Proof) StepResult {
	dir := pr.RepoDir
	on, err := f.Run(ctx, dir, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return leftf("main checkout %s: left: it is on no branch", dir)
	}
	if b := gitLine(on); b != pr.Default {
		return leftf("main checkout %s: left: it is on %s, not %s", dir, b, pr.Default)
	}
	status, err := f.Run(ctx, dir, "git", "status", "--porcelain", "-z", "--no-renames", "--untracked-files=no")
	if err != nil {
		return failedf("main checkout %s: left: its status was not read: %v", dir, err)
	}
	if n := bytes.Count(status, []byte{0}); n > 0 {
		return leftf("main checkout %s: left: %d uncommitted change(s)", dir, n)
	}
	if f.DryRun {
		return leftf("main checkout %s: would fast-forward %s (git pull --ff-only)", dir, pr.Default)
	}
	before, _ := f.Run(ctx, dir, "git", "rev-parse", "HEAD")
	if _, err := f.Run(ctx, dir, "git", "pull", "--ff-only", "--no-rebase"); err != nil {
		return failedf("main checkout %s: left: git pull --ff-only failed: %v", dir, err)
	}
	after, _ := f.Run(ctx, dir, "git", "rev-parse", "HEAD")
	if gitLine(before) == gitLine(after) {
		return leftf("main checkout %s: %s already up to date at %s", dir, pr.Default, short(gitLine(after)))
	}
	return leftf("main checkout %s: fast-forwarded %s %s..%s", dir, pr.Default, short(gitLine(before)), short(gitLine(after)))
}

// short is commit id c cut to twelve characters.
func short(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// CloseFinished closes terminal session ref, recorded for step s's session
// of a worker qm finish marked done, with the checks qm down makes before a
// close (closable): listed again right before, a running session's tab only
// when it runs that session (a tmux window when its one pane's foreground is
// its process), a not running one's only when it is an idle shell. A tab
// that now runs something else is left open and its ref dropped; the
// returned ref then carries the Warning (IsLeftOpen). A ref that the
// listing no longer shows is dropped. The caller holds the project's Lock
// and saves p.
func CloseFinished(ctx context.Context, p *Project, s Step, ref launch.SessionRef, terms Terminals) (launch.SessionRef, error) {
	id := s.SessionID
	l, found, err := relisted(ctx, p, ref, terms)
	if err != nil {
		return launch.SessionRef{}, fmt.Errorf("%s %s not closed: the workspace %s was not listed: %w", ref.Launcher, ref.ID, p.Workspace, err)
	}
	if !found {
		p.drop(id)
		if why := movedWhy(ctx, terms, p.Workspace, ref); why != "" {
			return leftRef(ref, why), nil
		}
		return launch.SessionRef{}, nil
	}
	if l.Linked {
		p.drop(id)
		return leftRef(ref, launch.LinkedWhy), nil
	}
	runs := closable(s, l)
	if runs && s.Action == Leave && l.Launcher == "tmux" {
		runs, err = terms.PaneRuns(ctx, l, id)
		if errors.Is(err, launch.ErrGone) {
			p.drop(id)
			return launch.SessionRef{}, nil
		}
		if err != nil {
			return launch.SessionRef{}, fmt.Errorf("tmux %s not closed: the foreground of its pane not read: %w", ref.ID, err)
		}
	}
	if !runs {
		p.drop(id)
		left := stored(ref)
		left.Warning = LeftOpen
		return left, nil
	}
	err = terms.Close(ctx, l)
	if c := (*launch.ChangedError)(nil); errors.As(err, &c) {
		p.drop(id)
		return leftRef(ref, c.Why), nil
	}
	if err != nil && !errors.Is(err, launch.ErrGone) {
		return launch.SessionRef{}, fmt.Errorf("close %s %s: %w", ref.Launcher, ref.ID, err)
	}
	p.drop(id)
	return stored(ref), nil
}

// NewestWorker is the worker of cands named name that MarkDone marks: the
// latest start, a tie going to the later listed.
func NewestWorker(cands []Candidate, name string) (Candidate, bool) {
	var newest *Candidate
	for i := range cands {
		c := &cands[i]
		if c.Role == RoleWorker && c.Name == name && (newest == nil || !c.Started.Before(newest.Started)) {
			newest = c
		}
	}
	if newest == nil {
		return Candidate{}, false
	}
	return *newest, true
}

// unescapeLsof undoes the escapes lsof prints a name's unprintable bytes
// with: \b \f \n \r \t \\, \xHH, and ^X for a control character.
func unescapeLsof(s string) string {
	if !strings.ContainsAny(s, "\\^") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+3 < len(s) && s[i+1] == 'x' && isHex(s[i+2]) && isHex(s[i+3]):
			n, _ := strconv.ParseUint(s[i+2:i+4], 16, 8)
			b.WriteByte(byte(n))
			i += 3
		case c == '\\' && i+1 < len(s) && strings.IndexByte("bfnrt\\", s[i+1]) >= 0:
			b.WriteByte(map[byte]byte{'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', '\\': '\\'}[s[i+1]])
			i++
		case c == '^' && i+1 < len(s) && s[i+1] >= '@' && s[i+1] <= '_':
			b.WriteByte(s[i+1] - '@')
			i++
		case c == '^' && i+1 < len(s) && s[i+1] == '?':
			b.WriteByte(0x7f)
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// parseLsof reads lsof -F pcn output: a p line starts a process, its c line
// names the command, an n line is the folder of its cwd file.
func parseLsof(out []byte) []ProcCwd {
	var procs []ProcCwd
	var cur ProcCwd
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch v := line[1:]; line[0] {
		case 'p':
			pid, _ := strconv.Atoi(v)
			cur = ProcCwd{PID: pid}
		case 'c':
			cur.Cmd = v
		case 'n':
			if cur.PID > 0 {
				cur.Cwd, cur.Raw = v, ""
				if d := unescapeLsof(v); d != v {
					cur.Cwd, cur.Raw = d, v
				}
				procs = append(procs, cur)
			}
		}
	}
	return procs
}
