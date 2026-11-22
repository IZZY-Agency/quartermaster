package outcomes

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/route"
)

func fixture(t *testing.T, name string) Transcript {
	t.Helper()
	f, err := os.Open("testdata/transcripts/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr, err := ParseTranscript(f)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestDedupByMessageID(t *testing.T) {
	tr := fixture(t, "opus-dedup.jsonl")
	if tr.PeakContext != 15000 {
		t.Errorf("peak context = %d, want 15000 (one message over 3 lines)", tr.PeakContext)
	}
	if len(tr.PRURLs) != 1 || tr.PRURLs[0] != "https://github.com/example/repo/pull/7" {
		t.Errorf("PRURLs = %v", tr.PRURLs)
	}
}

func TestCostOpus(t *testing.T) {
	// 1000*4 + 2000*20 + 10000*0.2 + 4000*5 = 66000 per million.
	if got := fixture(t, "opus-dedup.jsonl").Cost; math.Abs(got-0.066) > 1e-9 {
		t.Errorf("cost = %v, want 0.066", got)
	}
}

func TestCompactionCount(t *testing.T) {
	tr := fixture(t, "compactions.jsonl")
	if tr.Compactions != 2 {
		t.Errorf("compactions = %d, want 2", tr.Compactions)
	}
	if tr.PeakContext != 1000 {
		t.Errorf("peak = %d, want 1000", tr.PeakContext)
	}
}

func worker(name, typ, model, effort string) route.Record {
	return route.Record{Kind: "worker", Project: "/p", Branch: "b-" + name, Name: name, Type: typ,
		Decision: &route.Decision{Kind: "worker", Row: "row-" + name, Tier: route.Tier{Model: model, Effort: effort}}}
}

func review(branch string, round, p1, p2 int) route.Record {
	d := `{"verdict":"changes","p1":` + string(rune('0'+p1)) + `,"p2":` + string(rune('0'+p2)) + `,"p3":0}`
	return route.Record{Kind: "review", Project: "/p", Branch: branch, Round: round, Detail: d}
}

func input(recs []route.Record, b *Baseline) Input {
	return Input{
		Records:  recs,
		Worker:   func(route.Record) (Transcript, bool) { return Transcript{Cost: 1}, true },
		Session:  func(string) (Transcript, bool) { return Transcript{}, false },
		PR:       func(string) (PR, bool) { return PR{}, false },
		Baseline: b,
	}
}

func TestFlagWhenMoreRounds(t *testing.T) {
	recs := []route.Record{
		worker("a", "feature", "sonnet", "medium"),
		worker("b", "feature", "opus", "high"),
		worker("c", "feature", "sonnet", "low"),
		review("b-a", 3, 0, 0),
		review("b-b", 3, 0, 0),
		review("b-c", 1, 0, 0),
	}
	base := &Baseline{Rounds: map[string]float64{"feature": 2}, Findings: map[string]float64{"feature": 5}}
	rep := Build(input(recs, base))
	if len(rep.Flags) != 1 || !strings.HasPrefix(rep.Flags[0], "flag: row-a") {
		t.Fatalf("flags = %v, want only row-a (opus/high is not a downgrade, c is within the median)", rep.Flags)
	}
	// More P1+P2 than the median also flags.
	recs = []route.Record{worker("d", "feature", "sonnet", "high"), review("b-d", 1, 2, 2)}
	rep = Build(input(recs, &Baseline{Rounds: map[string]float64{"feature": 2}, Findings: map[string]float64{"feature": 3}}))
	if len(rep.Flags) != 1 {
		t.Errorf("flags = %v, want one for P1+P2", rep.Flags)
	}
	// No baseline, no flags.
	if rep := Build(input(recs, nil)); len(rep.Flags) != 0 {
		t.Errorf("flags without a baseline: %v", rep.Flags)
	}
}

func TestSoftenedListed(t *testing.T) {
	recs := []route.Record{
		{Kind: "guard", Rule: "read-budget", Action: "softened", Detail: "denied three times", Time: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)},
		{Kind: "guard", Rule: "write-note", Action: "denied"},
	}
	rep := Build(input(recs, nil))
	if len(rep.Softened) != 1 || rep.Softened[0].Rule != "read-budget" {
		t.Fatalf("softened = %v", rep.Softened)
	}
	var b bytes.Buffer
	rep.Write(&b)
	if !strings.Contains(b.String(), "GUARD SOFTENINGS") || !strings.Contains(b.String(), "read-budget") {
		t.Errorf("output lacks the softening:\n%s", b.String())
	}
}

func TestMergedLinesAndCostPerLine(t *testing.T) {
	in := input([]route.Record{worker("a", "feature", "opus", "high")}, nil)
	in.Worker = func(route.Record) (Transcript, bool) { return Transcript{Cost: 10, PRURLs: []string{"u1", "u2"}}, true }
	in.PR = func(u string) (PR, bool) {
		if u == "u1" {
			return PR{Additions: 80, Deletions: 20, Files: 3, Merged: true}, true
		}
		return PR{Additions: 500, Deletions: 0, Merged: false}, true
	}
	rep := Build(in)
	if got := rep.Tiers[0].CostPerLine(); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("cost per merged line = %v, want 0.1", got)
	}
	if rep.Workers[0].PRs != 2 || rep.Workers[0].Lines != 600 {
		t.Errorf("worker = %+v", rep.Workers[0])
	}
}

func TestComputeBaselineMedians(t *testing.T) {
	cut := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	before := cut.Add(-time.Hour)
	recs := []route.Record{
		{Time: before, Kind: "worker", Project: "/p", Branch: "x", Type: "feature"},
		{Time: before.Add(time.Minute), Kind: "worker", Project: "/p", Branch: "y", Type: "feature"},
		{Time: before.Add(2 * time.Minute), Kind: "review", Project: "/p", Branch: "x", Detail: `{"p1":4,"p2":0,"p3":0}`},
		{Time: before.Add(2 * time.Minute), Kind: "review", Project: "/p", Branch: "y", Detail: `{"p1":0,"p2":2,"p3":0}`},
		{Time: cut.Add(time.Hour), Kind: "worker", Project: "/p", Branch: "z", Type: "feature"},
	}
	b := ComputeBaseline(BaselineInput{Records: recs, Before: cut, Sessions: []BaselineSession{
		{Type: "feature", Transcript: Transcript{Cost: 2}}, {Type: "", Transcript: Transcript{Cost: 4}}, {Type: "feature", Transcript: Transcript{Cost: 6}},
	}})
	if b.Cost["feature"] != 4 || b.Cost["untyped"] != 4 || b.Findings["feature"] != 3 || b.Workers != 3 || b.Total != 12 {
		t.Errorf("baseline = %+v", b)
	}
}

func TestCoordinatorRows(t *testing.T) {
	recs := []route.Record{
		{Kind: "guard", Session: "coord-1", Action: "denied"},
		{Kind: "review", Session: "coord-1"},
		{Kind: "guard", Session: "coord-2"},
		{Kind: "guard", Session: "gone"},
	}
	in := input(recs, nil)
	in.Session = func(id string) (Transcript, bool) {
		if id == "gone" {
			return Transcript{}, false
		}
		return Transcript{Cost: 5, Compactions: 2, PeakContext: 900}, true
	}
	rep := Build(in)
	if len(rep.Coordinators) != 2 || rep.Coordinators[0].Session != "coord-1" || rep.Coordinators[0].Compactions != 2 || rep.Coordinators[1].PeakContext != 900 {
		t.Errorf("coordinators = %+v", rep.Coordinators)
	}
}

func TestNoBaselineForTypeIsSaid(t *testing.T) {
	recs := []route.Record{worker("a", "docs", "sonnet", "low")}
	rep := Build(input(recs, &Baseline{Rounds: map[string]float64{"feature": 1}, Findings: map[string]float64{}}))
	var got string
	for _, n := range rep.Notes {
		got += n + "\n"
	}
	if !strings.Contains(got, "no baseline for type \"docs\"") || len(rep.Flags) != 0 {
		t.Errorf("notes = %q flags = %v", got, rep.Flags)
	}
}

func TestOverallMedianFallback(t *testing.T) {
	recs := []route.Record{worker("a", "docs", "sonnet", "low"), review("b-a", 4, 0, 0)}
	rep := Build(input(recs, &Baseline{Rounds: map[string]float64{"feature": 1}, RoundsAll: 1, HasRounds: true}))
	if len(rep.Flags) != 1 || !strings.Contains(rep.Flags[0], "overall") {
		t.Errorf("flags = %v, want one against the overall median", rep.Flags)
	}
}

func TestReusedBranchCountsReviewOnce(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	w1, w2 := worker("a", "feature", "opus", "high"), worker("c", "feature", "opus", "high")
	w1.Branch, w2.Branch = "shared", "shared"
	w1.Time, w2.Time = t0, t0.Add(48*time.Hour)
	r1, r2 := review("shared", 2, 1, 0), review("shared", 1, 0, 1)
	r1.Time, r2.Time = t0.Add(time.Hour), t0.Add(49*time.Hour)
	rep := Build(input([]route.Record{w1, r1, w2, r2}, nil))
	a, c := rep.Workers[0], rep.Workers[1]
	if a.Rounds != 2 || a.Findings != 1 || c.Rounds != 1 || c.Findings != 1 {
		t.Errorf("workers = %+v %+v", a, c)
	}
}

func TestUnknownTierIsNotADowngrade(t *testing.T) {
	if belowOpusHigh(route.Tier{Model: "newmodel", Effort: "high"}) {
		t.Error("an unknown model was called a downgrade")
	}
}

func TestIncompleteCountsPrinted(t *testing.T) {
	in := input([]route.Record{worker("a", "feature", "opus", "high"), worker("b", "feature", "opus", "high")}, nil)
	in.Worker = func(r route.Record) (Transcript, bool) {
		if r.Name == "b" {
			return Transcript{}, false
		}
		return Transcript{PRURLs: []string{"u"}, UnknownModels: 3}, true
	}
	rep := Build(in)
	var b bytes.Buffer
	rep.Write(&b)
	if rep.UnresolvedWorkers != 1 || rep.UnresolvedPRs != 1 || rep.UnknownModels != 3 || !strings.Contains(b.String(), "incomplete: 1 worker(s)") {
		t.Errorf("%+v\n%s", rep, b.String())
	}
}

func TestBaselineSkipsNoRounds(t *testing.T) {
	b := ComputeBaseline(BaselineInput{Sessions: []BaselineSession{
		{Type: "feature", Rounds: 2}, {Type: "feature", Rounds: -1}, {Type: "feature", Rounds: 4},
	}})
	if b.Rounds["feature"] != 3 || !b.HasRounds || b.HasFindings {
		t.Errorf("baseline = %+v", b)
	}
}

func TestNoNotesWithoutBaseline(t *testing.T) {
	rep := Build(input([]route.Record{worker("a", "docs", "sonnet", "low"), review("b-a", 4, 0, 0)}, nil))
	if len(rep.Notes) != 0 {
		t.Errorf("notes without --baseline: %v", rep.Notes)
	}
}

func agentReview(branch string, round int) route.Record {
	return route.Record{Kind: "review", Project: "/p", Branch: branch, Round: round, Detail: `{"verdict":"agent","p1":0,"p2":0,"p3":0}`}
}

func TestAgentReviewsCountRoundsNotFindings(t *testing.T) {
	// d's only reviews are Agent reviews: rounds count, findings are unknown,
	// so a findings median of 0 must not read d as clean or compare it.
	recs := []route.Record{worker("d", "feature", "sonnet", "high"), agentReview("b-d", 1), agentReview("b-d", 2)}
	rep := Build(input(recs, &Baseline{Rounds: map[string]float64{"feature": 5}, Findings: map[string]float64{"feature": -1}}))
	d := rep.Workers[0]
	if d.Rounds != 2 || !d.FindingsUnread || len(rep.Flags) != 0 {
		t.Errorf("worker %+v, flags %v; want 2 rounds, findings unread, no flag", d, rep.Flags)
	}
	var out bytes.Buffer
	rep.Write(&out)
	row := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "d ") {
			row = l
		}
	}
	if f := strings.Fields(row); len(f) == 0 || f[len(f)-1] != "-" {
		t.Errorf("worker row %q, want P1+P2 shown as -", row)
	}
	// A worker with a read review as well keeps its findings.
	recs = append(recs, review("b-d", 3, 1, 1))
	if d := Build(input(recs, nil)).Workers[0]; d.FindingsUnread || d.Findings != 2 || d.Rounds != 3 {
		t.Errorf("mixed reviews: %+v, want findings 2 from the read review, 3 rounds", d)
	}

	// Baseline: the agent-only worker is left out of the findings median.
	cut := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	before := cut.Add(-time.Hour)
	brecs := []route.Record{
		{Time: before, Kind: "worker", Project: "/p", Branch: "x", Type: "feature"},
		{Time: before, Kind: "worker", Project: "/p", Branch: "y", Type: "feature"},
		{Time: before.Add(time.Minute), Kind: "review", Project: "/p", Branch: "x", Detail: `{"verdict":"changes","p1":4,"p2":0,"p3":0}`},
		{Time: before.Add(time.Minute), Kind: "review", Project: "/p", Branch: "y", Detail: `{"verdict":"agent","p1":0,"p2":0,"p3":0}`},
	}
	b := ComputeBaseline(BaselineInput{Records: brecs, Before: cut})
	if b.Findings["feature"] != 4 || b.FindingsAll != 4 {
		t.Errorf("findings medians %v / %v, want 4: the agent-only worker is left out", b.Findings, b.FindingsAll)
	}
}

func TestUnparsedDetailCountsUnread(t *testing.T) {
	bad := route.Record{Kind: "review", Project: "/p", Branch: "b-e", Round: 1, Detail: "agterm 42"}
	rep := Build(input([]route.Record{worker("e", "feature", "sonnet", "high"), bad}, &Baseline{Rounds: map[string]float64{"feature": 5}, Findings: map[string]float64{"feature": -1}}))
	if e := rep.Workers[0]; e.Rounds != 1 || !e.FindingsUnread || len(rep.Flags) != 0 {
		t.Errorf("worker %+v flags %v: a review Detail that is not JSON must count as unread", e, rep.Flags)
	}
}

func TestReviewerSessionIsAWorkerNotATarget(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	impl := worker("i", "feature", "opus", "high")
	impl.Branch, impl.Time = "shared", t0
	round := route.Record{Kind: "review", Project: "/p", Branch: "shared", Round: 1, Time: t0.Add(time.Hour), Detail: `{"verdict":"session","p1":0,"p2":0,"p3":0}`}
	rev := worker("r", "review", "sonnet", "medium")
	rev.Branch, rev.Time = "shared", t0.Add(time.Hour+time.Microsecond)
	later := review("shared", 2, 1, 0)
	later.Time = t0.Add(2 * time.Hour)
	rep := Build(input([]route.Record{impl, round, rev, later}, &Baseline{Rounds: map[string]float64{}, Findings: map[string]float64{}, HasRounds: true, HasFindings: true}))
	if len(rep.Workers) != 2 {
		t.Fatalf("workers = %+v, want the implementer and the reviewer", rep.Workers)
	}
	i, r := rep.Workers[0], rep.Workers[1]
	if i.Rounds != 2 || i.Findings != 1 || i.FindingsUnread || r.Rounds != 0 || r.Flag != "" {
		t.Errorf("implementer %+v reviewer %+v: both reviews belong to the implementer", i, r)
	}
}
