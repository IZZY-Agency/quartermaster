package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/privacy"
	"github.com/IZZY-Agency/quartermaster/internal/reviewin"
	"github.com/IZZY-Agency/quartermaster/internal/route"
	"github.com/IZZY-Agency/quartermaster/internal/spawn"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// fakeJevEnv names a fixture file whose answers replace Jev; tests use it.
// Every decision made with it carries reasonFakeJev.
const (
	fakeJevEnv    = "QM_FAKE_JEV"
	reasonFakeJev = "fake Jev in use (QM_FAKE_JEV)"
)

// codexLook finds the codex command for limits.Codex; tests replace it.
var codexLook = exec.LookPath

const routeUsage = `usage:
  qm route worker --brief FILE --type T --size S|M|L [--hint m/e] [--force m/e --why TEXT] [--repo DIR] [--json]
  qm route review --prompt FILE [--round N] [--base REF] [--repo DIR] [--json]
`

// workerTypes are the values of qm route worker --type (design 5.3).
var workerTypes = []string{"feature", "fix", "fix-pass", "docs", "release-paperwork", "research", "migration", "ui", "review"}

// errUsage marks bad input: qm route exits 2 on it.
var errUsage = errors.New("usage")

// runRoute prints the routing decision for a worker or a review. It is a
// preview: it never writes the decision log (the spawn commands do, through
// declog.AppendDecision). Exit 2 on bad input, 1 when routing fails.
func runRoute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, routeUsage)
		return 2
	}
	if isHelp(args) {
		_, _ = fmt.Fprint(stdout, routeUsage)
		return 0
	}
	var err error
	switch args[0] {
	case "worker":
		err = routeWorker(args[1:], stdout, stderr)
	case "review":
		err = routeReview(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "qm route: unknown kind %q\n%s", args[0], routeUsage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		_, _ = fmt.Fprintf(stderr, "qm route: %v\n%s", err, routeUsage)
		return 2
	default:
		_, _ = fmt.Fprintf(stderr, "qm route: %v\n", err)
		return 1
	}
}

func routeWorker(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("qm route worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	briefFile := fs.String("brief", "", "the worker brief file")
	typ := fs.String("type", "", "task type: "+strings.Join(workerTypes, ", "))
	size := fs.String("size", "", "S, M or L")
	hint := fs.String("hint", "", "the coordinator's pick, model/effort")
	force := fs.String("force", "", "override the table, model/effort (needs --why)")
	why := fs.String("why", "", "why the tier is forced")
	repo := fs.String("repo", ".", "the repository the worker works in")
	asJSON := fs.Bool("json", false, "print the decision as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	switch {
	case *briefFile == "":
		return fmt.Errorf("%w: --brief is required", errUsage)
	case !slices.Contains(workerTypes, *typ):
		return fmt.Errorf("%w: --type %q is not one of %s", errUsage, *typ, strings.Join(workerTypes, ", "))
	case *force != "" && strings.TrimSpace(*why) == "":
		return fmt.Errorf("%w: --force needs --why", errUsage)
	case *force == "" && *why != "":
		return fmt.Errorf("%w: --why goes with --force", errUsage)
	}
	in := route.WorkerInput{Type: *typ, Size: *size, ForceWhy: strings.TrimSpace(*why)}
	var err error
	if in.Hint, err = parseTier("--hint", *hint); err != nil {
		return err
	}
	if in.Force, err = parseTier("--force", *force); err != nil {
		return err
	}
	brief, err := os.ReadFile(*briefFile)
	if err != nil {
		return err
	}
	in.Brief = string(brief)
	if in.Repo, err = workerRepo(*repo); err != nil {
		return err
	}

	deps, err := routeDeps()
	if err != nil {
		return err
	}
	if *typ == "review" {
		// A review session is routed as qm spawn worker routes it.
		return routeReviewSession(*briefFile, in, deps, claudeState(stderr), stdout, *asJSON)
	}
	d, err := route.DecideWorker(context.Background(), in, deps.ask, deps.gate, claudeState(stderr))
	if err != nil {
		return routeError(err)
	}
	return printDecision(stdout, deps.mark(d), *asJSON)
}

// routeReviewSession previews qm spawn worker --type review: the same
// spawn.Route call, so the review table is used and the Codex tier dropped.
func routeReviewSession(briefFile string, in route.WorkerInput, deps routingDeps, claude limits.ClaudeState, stdout io.Writer, asJSON bool) error {
	sd := spawn.Deps{
		Ask: deps.ask, Gate: deps.gate, Claude: claude, Mark: deps.mark,
		Codex: func() limits.CodexState {
			st, err := limits.Codex(codexLook)
			if err != nil {
				return limits.CodexState{Reason: "Codex usage unreadable: " + err.Error()}
			}
			return st
		},
	}
	req := spawn.WorkerRequest{Brief: briefFile, Type: in.Type, Size: in.Size, Cwd: in.Repo, Hint: in.Hint, Force: in.Force, ForceWhy: in.ForceWhy}
	d, _, err := spawn.Route(context.Background(), req, in.Repo, sd)
	if err != nil {
		return routeError(err)
	}
	return printDecision(stdout, d, asJSON)
}

func routeReview(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("qm route review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	promptFile := fs.String("prompt", "", "the review prompt file")
	round := fs.Int("round", 0, "review round (default: 1 plus the reviews logged for this branch)")
	base := fs.String("base", "", "the ref the diff under review is taken against (default: origin/HEAD's branch, else "+reviewin.DefaultBase+")")
	repo := fs.String("repo", ".", "the repository under review")
	asJSON := fs.Bool("json", false, "print the decision as JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	switch {
	case *promptFile == "":
		return fmt.Errorf("%w: --prompt is required", errUsage)
	case *round < 0:
		return fmt.Errorf("%w: --round %d is negative", errUsage, *round)
	}
	in, err := reviewin.For(*repo, *base, *promptFile, *round)
	if err != nil {
		return err
	}

	deps, err := routeDeps()
	if err != nil {
		return err
	}
	codex, err := limits.Codex(codexLook)
	if err != nil {
		codex = limits.CodexState{Reason: "Codex usage unreadable: " + err.Error()}
	}
	d, err := route.DecideReview(context.Background(), in, deps.ask, deps.gate, codex, claudeState(stderr))
	if err != nil {
		return routeError(err)
	}
	return printDecision(stdout, deps.mark(d), *asJSON)
}

// parseFlags parses args and rejects positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}
	return nil
}

// parseTier reads "model/effort"; "" is no tier. The model and effort are
// checked by route.
func parseTier(name, s string) (*route.Tier, error) {
	if s == "" {
		return nil, nil
	}
	m, e, ok := strings.Cut(s, "/")
	if !ok || m == "" || e == "" {
		return nil, fmt.Errorf("%w: %s %q is not model/effort", errUsage, name, s)
	}
	return &route.Tier{Model: m, Effort: e}, nil
}

// routeError marks a route error as bad input: DecideWorker and DecideReview
// fail only on it once routeDeps has supplied a gate.
func routeError(err error) error {
	return fmt.Errorf("%w: %v", errUsage, err)
}

// workerRepo is the repository a worker works in: its top level, or, outside
// a repository, the folder's absolute path.
func workerRepo(dir string) (string, error) {
	if fi, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("%w: --repo: %v", errUsage, err)
	} else if !fi.IsDir() {
		return "", fmt.Errorf("%w: --repo %s is not a folder", errUsage, dir)
	}
	if top, _, err := reviewin.Repo(dir); err == nil {
		return top, nil
	}
	return filepath.Abs(dir)
}

// routingDeps is what a routing decision needs besides its input.
type routingDeps struct {
	ask  route.Asker
	gate route.Gater
	fake bool // ask answers from a QM_FAKE_JEV fixture
}

// mark adds reasonFakeJev to a decision made while QM_FAKE_JEV is set.
func (r routingDeps) mark(d route.Decision) route.Decision {
	if r.fake {
		d.Reasons = append(d.Reasons, reasonFakeJev)
	}
	return d
}

// routeDeps loads the config and builds the Jev asker and the privacy gate.
// The asker is a fake from QM_FAKE_JEV when set, else a TypeSafe client when
// there is a key, else an untyped nil: Jev off. It is never a nil
// *typesafe.Client inside the interface, which would read as Jev on.
func routeDeps() (routingDeps, error) {
	cfg, err := config.Load()
	if err != nil {
		return routingDeps{}, err
	}
	gate := privacy.New(privacy.Options{
		Helper:     privacy.FindHelper(),
		Gitleaks:   privacy.FindGitleaks(),
		LocalModel: cfg.Privacy.LocalModel,
		Clients:    cfg.Privacy.Clients,
	})
	if p := os.Getenv(fakeJevEnv); p != "" {
		ask, err := loadFakeJev(p)
		return routingDeps{ask: ask, gate: gate, fake: true}, err
	}
	if c := typesafe.New(cfg.Key()); c != nil {
		return routingDeps{ask: c, gate: gate}, nil
	}
	return routingDeps{gate: gate}, nil
}

// claudeState is the Claude limit state; one that cannot be read is
// reported on stderr and treated as unknown (no downshift).
func claudeState(stderr io.Writer) limits.ClaudeState {
	st, err := limits.Claude()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "qm route: Claude limits unknown: %v\n", err)
		return limits.ClaudeState{}
	}
	return st
}

// printDecision prints d: indented JSON, or decisionLine.
func printDecision(stdout io.Writer, d route.Decision, asJSON bool) error {
	text := decisionLine(d)
	if asJSON {
		b, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return err
		}
		text = string(b)
	}
	if _, err := fmt.Fprintln(stdout, text); err != nil {
		return fmt.Errorf("writing the decision: %w", err)
	}
	return nil
}

// decisionLine is the decision in one line: "opus/high (row risk): reasons",
// with the Codex tier after the Claude one when a review goes to Codex.
func decisionLine(d route.Decision) string {
	tier := d.Tier.Model + "/" + d.Tier.Effort
	if d.Codex != nil {
		tier += ", Codex " + d.Codex.Model + "/" + d.Codex.Effort
	}
	return fmt.Sprintf("%s (row %s): %s", tier, d.Row, strings.Join(d.Reasons, "; "))
}

// fakeJev answers every request from a fixture: question id to answer.
type fakeJev map[string]struct {
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
}

func loadFakeJev(path string) (route.Asker, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fakeJevEnv, err)
	}
	var f fakeJev
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s %s: %w", fakeJevEnv, path, err)
	}
	return f, nil
}

// Ask implements route.Asker; a question the fixture lacks is an error, as a
// reply without an answer is from Jev.
func (f fakeJev) Ask(_ context.Context, _ any, qs map[string]typesafe.Question) (map[string]typesafe.Answer, error) {
	out := make(map[string]typesafe.Answer, len(qs))
	for id, q := range qs {
		a, ok := f[id]
		if !ok {
			return nil, fmt.Errorf("%s: no answer for %q", fakeJevEnv, id)
		}
		out[id] = typesafe.Answer{Type: q.Type, Noul: a.Noul, Choice: a.Choice, Score: a.Score, Probabilities: a.Probabilities}
	}
	return out, nil
}
