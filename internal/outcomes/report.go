package outcomes

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/plain"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

// PR is what gh pr view reports about a pull request.
type PR struct {
	Additions, Deletions, Files int
	Merged                      bool
}

// Lines is the lines the pull request changed.
func (p PR) Lines() int { return p.Additions + p.Deletions }

// Input is everything Build reads. The three lookups are functions so the
// report can be tested without Claude Code's files or gh.
type Input struct {
	// Records are the decision log records of the window.
	Records []route.Record
	// Worker returns the transcript of the session a worker Record launched.
	Worker func(r route.Record) (Transcript, bool)
	// Session returns the transcript of a Claude session id.
	Session func(id string) (Transcript, bool)
	// PR looks a pull request up by URL.
	PR func(url string) (PR, bool)
	// Baseline holds the medians the flags compare with; nil means no flags.
	Baseline *Baseline
}

// Baseline holds medians per Type. Rounds and Findings come from decision
// records; Cost, Context and Compactions from transcripts.
type Baseline struct {
	Workers     int
	Cost        map[string]float64
	Context     map[string]float64
	Compactions map[string]float64
	Rounds      map[string]float64
	Findings    map[string]float64
	// RoundsAll and FindingsAll are the overall medians, used when a Type has
	// no median of its own; HasRounds and HasFindings say whether there is data.
	RoundsAll, FindingsAll float64
	HasRounds, HasFindings bool
	Total                  float64
	UnknownModels          int
}

// Worker is one worker decision with its outcome.
type Worker struct {
	Name, Type, Row string
	Tier            route.Tier
	Cost            float64
	PeakContext     int
	Compactions     int
	PRs             int
	Lines           int
	Files           int
	MergedLines     int
	Rounds          int
	Findings        int // P1 + P2
	// FindingsUnread: every review of this worker was an Agent-tool review
	// (verdict "agent"), whose findings qm never reads, so Findings is
	// unknown, not 0, and is left out of the P1+P2 medians and comparisons.
	FindingsUnread bool
	Flag           string
}

// Coordinator is a session that spawned workers or logged records.
type Coordinator struct {
	Session     string
	Cost        float64
	Compactions int
	PeakContext int
}

// Row is one tier of the by-tier table.
type Row struct {
	Tier        route.Tier
	Workers     int
	Cost        float64
	MergedLines int
	Compactions int
}

// CostPerLine is dollars per merged line, 0 when nothing merged.
func (r Row) CostPerLine() float64 {
	if r.MergedLines == 0 {
		return 0
	}
	return r.Cost / float64(r.MergedLines)
}

// Report is the built report.
type Report struct {
	Workers      []Worker
	Tiers        []Row
	Coordinators []Coordinator
	Softened     []route.Record
	Flags        []string
	Total        float64
	// Notes say what the report could not compare or resolve.
	Notes []string
	// UnresolvedWorkers are worker decisions with no transcript; UnresolvedPRs
	// are pull requests gh could not describe; UnknownModels counts messages
	// of a model with no price. Their cost or lines are missing from the totals.
	UnresolvedWorkers, UnresolvedPRs, UnknownModels int
}

// reviewStats is the review rounds and P1+P2 findings of a worker decision;
// read and unread count its reviews whose findings were and were not read.
type reviewStats struct{ rounds, findings, read, unread int }

// findingsUnread: the worker had reviews, and none of them had its findings
// read.
func (s reviewStats) findingsUnread() bool { return s.unread > 0 && s.read == 0 }

// unreadVerdicts mark a review whose findings qm never reads: "agent", a
// review started through the Agent tool and logged by the reviewer-model hook,
// and "session", a reviewer session started by qm spawn worker --type review.
// Each counts as a round.
var unreadVerdicts = map[string]bool{"agent": true, "session": true}

// reviewerType is the worker Type of a reviewer session; such a worker is
// not the target of the reviews on its branch.
const reviewerType = "review"

type branchKey struct{ project, branch string }

// assignReviews gives each review record to the latest worker record on the
// same project and branch that is not newer than the review, so a branch
// reused by two workers does not count one review twice. The result is keyed
// by the worker record's index in recs.
func assignReviews(recs []route.Record) map[int]reviewStats {
	workers := map[branchKey][]int{}
	for i, r := range recs {
		if r.Kind == "worker" && r.Type != reviewerType {
			k := branchKey{r.Project, r.Branch}
			workers[k] = append(workers[k], i)
		}
	}
	out := map[int]reviewStats{}
	for _, r := range recs {
		if r.Kind != "review" {
			continue
		}
		best := -1
		for _, j := range workers[branchKey{r.Project, r.Branch}] {
			if !recs[j].Time.After(r.Time) && (best < 0 || !recs[j].Time.Before(recs[best].Time)) {
				best = j
			}
		}
		if best < 0 {
			continue
		}
		s := out[best]
		if r.Round > s.rounds {
			s.rounds = r.Round
		}
		var d struct {
			Verdict string
			P1, P2  int
		}
		// A Detail that is not the review JSON says nothing of the
		// findings: unread, like an Agent review.
		if err := json.Unmarshal([]byte(r.Detail), &d); err != nil || unreadVerdicts[d.Verdict] {
			s.unread++
		} else {
			s.findings += d.P1 + d.P2
			s.read++
		}
		out[best] = s
	}
	return out
}

var modelRank = map[string]int{"haiku": 0, "sonnet": 1, "opus": 2, "fable": 3}
var effortRank = map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}

// belowOpusHigh reports whether t is a downgrade from opus/high. A tier with a
// model or effort this code does not know is not called a downgrade.
func belowOpusHigh(t route.Tier) bool {
	m, ok := modelRank[t.Model]
	e, ok2 := effortRank[t.Effort]
	if !ok || !ok2 {
		return false
	}
	if m != modelRank["opus"] {
		return m < modelRank["opus"]
	}
	return e < effortRank["high"]
}

// median returns the baseline median for a Type, else the overall median.
func median(byType map[string]float64, all float64, hasAll bool, ty string) (med float64, scope string, ok bool) {
	if v, found := byType[ty]; found {
		return v, "type " + ty, true
	}
	if hasAll {
		return all, "overall", true
	}
	return 0, "", false
}

// Build joins the records, transcripts and pull requests into a report.
func Build(in Input) Report {
	reviews := assignReviews(in.Records)
	var rep Report
	coord := map[string]bool{}
	var coordOrder []string
	noted := map[string]bool{}
	note := func(s string) {
		if !noted[s] {
			noted[s] = true
			rep.Notes = append(rep.Notes, s)
		}
	}
	for i, r := range in.Records {
		if r.Session != "" && !coord[r.Session] {
			coord[r.Session] = true
			coordOrder = append(coordOrder, r.Session)
		}
		if r.Kind == "guard" && r.Action == "softened" {
			rep.Softened = append(rep.Softened, r)
		}
		if r.Kind != "worker" || r.Decision == nil {
			continue
		}
		w := Worker{Name: r.Name, Type: r.Type, Row: r.Decision.Row, Tier: r.Decision.Tier}
		if t, ok := in.Worker(r); ok {
			w.Cost, w.PeakContext, w.Compactions = t.Cost, t.PeakContext, t.Compactions
			rep.UnknownModels += t.UnknownModels
			for _, u := range t.PRURLs {
				w.PRs++
				pr, ok := in.PR(u)
				if !ok {
					rep.UnresolvedPRs++
					continue
				}
				w.Lines += pr.Lines()
				w.Files += pr.Files
				if pr.Merged {
					w.MergedLines += pr.Lines()
				}
			}
		} else {
			rep.UnresolvedWorkers++
		}
		rs := reviews[i]
		w.Rounds, w.Findings, w.FindingsUnread = rs.rounds, rs.findings, rs.findingsUnread()
		if b := in.Baseline; b != nil && belowOpusHigh(w.Tier) && w.Type != reviewerType {
			label := w.Row
			if label == "" {
				label = w.Name
			}
			ty := w.Type
			if med, scope, ok := median(b.Rounds, b.RoundsAll, b.HasRounds, ty); !ok {
				note(fmt.Sprintf("no baseline for type %q: review rounds are not compared", ty))
			} else if float64(w.Rounds) > med {
				w.Flag = fmt.Sprintf("flag: %s (review rounds %d, baseline median %.1f, %s)", label, w.Rounds, med, scope)
			}
			if w.Flag == "" && !w.FindingsUnread {
				if med, scope, ok := median(b.Findings, b.FindingsAll, b.HasFindings, ty); !ok {
					note(fmt.Sprintf("no baseline for type %q: P1+P2 findings are not compared (they need decision records from before --before)", ty))
				} else if float64(w.Findings) > med {
					w.Flag = fmt.Sprintf("flag: %s (P1+P2 %d, baseline median %.1f, %s)", label, w.Findings, med, scope)
				}
			}
		}
		if w.Flag != "" {
			rep.Flags = append(rep.Flags, w.Flag)
		}
		rep.Total += w.Cost
		rep.Workers = append(rep.Workers, w)
	}
	byTier := map[route.Tier]*Row{}
	for _, w := range rep.Workers {
		row := byTier[w.Tier]
		if row == nil {
			row = &Row{Tier: w.Tier}
			byTier[w.Tier] = row
		}
		row.Workers++
		row.Cost += w.Cost
		row.MergedLines += w.MergedLines
		row.Compactions += w.Compactions
	}
	for _, r := range byTier {
		rep.Tiers = append(rep.Tiers, *r)
	}
	sort.Slice(rep.Tiers, func(i, j int) bool {
		a, b := rep.Tiers[i].Tier, rep.Tiers[j].Tier
		if a.Model != b.Model {
			return modelRank[a.Model] < modelRank[b.Model]
		}
		return effortRank[a.Effort] < effortRank[b.Effort]
	})
	for _, id := range coordOrder {
		if t, ok := in.Session(id); ok {
			rep.Coordinators = append(rep.Coordinators, Coordinator{id, t.Cost, t.Compactions, t.PeakContext})
			rep.UnknownModels += t.UnknownModels
		}
	}
	return rep
}

// Median is the median of vals, 0 for none.
func Median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	v := append([]float64(nil), vals...)
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// BaselineInput is what ComputeBaseline reads.
type BaselineInput struct {
	// Records are all decision records; those before Before supply the P1+P2
	// medians per Type.
	Records []route.Record
	Before  time.Time
	// Sessions are the worker transcripts started before Before.
	Sessions []BaselineSession
}

// BaselineSession is one worker transcript. Type comes from a decision record
// or the brief, "" when neither names one. Rounds is the number of review
// sessions on the worker's branch, -1 for a reviewer or a worker with no
// feature branch (it has no rounds).
type BaselineSession struct {
	Type   string
	Rounds int
	Transcript
}

// untyped is the Type of a transcript no record or brief gives a type.
const untyped = "untyped"

func medians(m map[string][]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		out[k] = Median(v)
	}
	return out
}

// ComputeBaseline takes medians per Type. Transcripts give cost, peak context,
// compactions and review rounds (reviewer sessions on the branch); P1+P2
// findings cannot be read reliably from a transcript, so they come only from
// decision records before Before.
func ComputeBaseline(in BaselineInput) *Baseline {
	cost, ctx, comp, rounds := map[string][]float64{}, map[string][]float64{}, map[string][]float64{}, map[string][]float64{}
	var allRounds []float64
	b := &Baseline{}
	for _, s := range in.Sessions {
		ty := s.Type
		if ty == "" {
			ty = untyped
		}
		cost[ty] = append(cost[ty], s.Cost)
		ctx[ty] = append(ctx[ty], float64(s.PeakContext))
		comp[ty] = append(comp[ty], float64(s.Compactions))
		if s.Rounds >= 0 {
			rounds[ty] = append(rounds[ty], float64(s.Rounds))
			allRounds = append(allRounds, float64(s.Rounds))
		}
		b.Total += s.Cost
		b.Workers++
		b.UnknownModels += s.UnknownModels
	}
	b.Cost, b.Context, b.Compactions, b.Rounds = medians(cost), medians(ctx), medians(comp), medians(rounds)
	b.RoundsAll, b.HasRounds = Median(allRounds), len(allRounds) > 0
	var before []route.Record
	for _, r := range in.Records {
		if r.Time.Before(in.Before) {
			before = append(before, r)
		}
	}
	rv := assignReviews(before)
	finds := map[string][]float64{}
	var allFinds []float64
	for i, r := range before {
		if r.Kind != "worker" || r.Type == reviewerType || rv[i].findingsUnread() {
			continue
		}
		ty := r.Type
		if ty == "" {
			ty = untyped
		}
		finds[ty] = append(finds[ty], float64(rv[i].findings))
		allFinds = append(allFinds, float64(rv[i].findings))
	}
	b.Findings = medians(finds)
	b.FindingsAll, b.HasFindings = Median(allFinds), len(allFinds) > 0
	return b
}

// Write prints the report as text.
func (rep Report) Write(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIER\tWORKERS\tCOST\tMERGED LINES\t$/LINE\tCOMPACTIONS")
	for _, r := range rep.Tiers {
		_, _ = fmt.Fprintf(tw, "%s/%s\t%d\t$%.2f\t%d\t$%.3f\t%d\n", r.Tier.Model, r.Tier.Effort, r.Workers, r.Cost, r.MergedLines, r.CostPerLine(), r.Compactions)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\nworkers: %d, API-equivalent cost $%.2f\n", len(rep.Workers), rep.Total)
	if len(rep.Workers) > 0 {
		_, _ = fmt.Fprintln(w, "\nWORKERS")
		tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tTYPE\tTIER\tCOST\tPEAK CONTEXT\tCOMPACTIONS\tPRS\tFILES\tLINES\tROUNDS\tP1+P2")
		for _, x := range rep.Workers {
			findings := strconv.Itoa(x.Findings)
			if x.FindingsUnread {
				findings = "-"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s/%s\t$%.2f\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", plain.Line(x.Name), plain.Line(x.Type), x.Tier.Model, x.Tier.Effort, x.Cost, x.PeakContext, x.Compactions, x.PRs, x.Files, x.Lines, x.Rounds, findings)
		}
		_ = tw.Flush()
	}
	if len(rep.Coordinators) > 0 {
		_, _ = fmt.Fprintln(w, "\nCOORDINATOR SESSIONS")
		tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "SESSION\tCOST\tCOMPACTIONS\tPEAK CONTEXT")
		for _, c := range rep.Coordinators {
			_, _ = fmt.Fprintf(tw, "%s\t$%.2f\t%d\t%d\n", plain.Line(c.Session), c.Cost, c.Compactions, c.PeakContext)
		}
		_ = tw.Flush()
	}
	if len(rep.Softened) > 0 {
		_, _ = fmt.Fprintln(w, "\nGUARD SOFTENINGS")
		for _, r := range rep.Softened {
			_, _ = fmt.Fprintf(w, "%s  %s  %s\n", r.Time.Format(time.RFC3339), plain.Line(r.Rule), strings.TrimSpace(plain.Line(r.Detail)))
		}
	}
	if rep.UnresolvedWorkers+rep.UnresolvedPRs+rep.UnknownModels > 0 {
		_, _ = fmt.Fprintf(w, "\nincomplete: %d worker(s) without a transcript, %d PR(s) gh could not describe, %d message(s) of an unknown model (their cost is not counted)\n", rep.UnresolvedWorkers, rep.UnresolvedPRs, rep.UnknownModels)
	}
	for _, n := range rep.Notes {
		_, _ = fmt.Fprintln(w, "note: "+n)
	}
	if len(rep.Flags) > 0 {
		_, _ = fmt.Fprintln(w, "")
		for _, f := range rep.Flags {
			_, _ = fmt.Fprintln(w, f)
		}
	}
}

// Write prints the baseline medians per Type.
func (b *Baseline) Write(w io.Writer) {
	_, _ = fmt.Fprintf(w, "baseline: %d worker sessions, API-equivalent cost $%.2f\n", b.Workers, b.Total)
	if b.UnknownModels > 0 {
		_, _ = fmt.Fprintf(w, "baseline: %d message(s) of an unknown model are not priced\n", b.UnknownModels)
	}
	types := make([]string, 0, len(b.Cost))
	for ty := range b.Cost {
		types = append(types, ty)
	}
	sort.Strings(types)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TYPE\tMEDIAN COST\tMEDIAN PEAK CONTEXT\tMEDIAN COMPACTIONS\tMEDIAN ROUNDS\tMEDIAN P1+P2")
	for _, ty := range types {
		_, _ = fmt.Fprintf(tw, "%s\t$%.2f\t%.0f\t%.1f\t%.1f\t%.1f\n", ty, b.Cost[ty], b.Context[ty], b.Compactions[ty], b.Rounds[ty], b.Findings[ty])
	}
	_ = tw.Flush()
}
