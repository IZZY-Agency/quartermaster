package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/config"
	"github.com/IZZY-Agency/quartermaster/internal/declog"
	"github.com/IZZY-Agency/quartermaster/internal/outcomes"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

const dateLayout = "2006-01-02"

func init() {
	commands["report"] = command{"print what workers cost and how their reviews went", runReport}
}

func runReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "", "only decisions on or after this day (YYYY-MM-DD)")
	baseline := fs.Bool("baseline", false, "compute medians per Type from worker transcripts before --before and flag against them (P1+P2 medians need decision records from before --before)")
	var projects stringList
	fs.Var(&projects, "project", "with --baseline: only transcripts whose project folder name contains this text (repeatable)")
	before := fs.String("before", "", "with --baseline: transcripts before this day (default: the day of the first decision record)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*before != "" || len(projects) > 0) && !*baseline {
		_, _ = fmt.Fprintln(stderr, "qm report: --before and --project need --baseline")
		return 2
	}
	var sinceT time.Time
	if *since != "" {
		t, err := time.ParseInLocation(dateLayout, *since, time.Local)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "qm report: --since: %v\n", err)
			return 2
		}
		sinceT = t
	}
	all, err := declog.Read(time.Time{})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "qm report: %v\n", err)
		return 1
	}
	var recs []route.Record
	for _, r := range all {
		if !r.Time.Before(sinceT) {
			recs = append(recs, r)
		}
	}
	cl, err := outcomes.DefaultClaude()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "qm report: %v\n", err)
		return 1
	}
	finder := &outcomes.WorkerFinder{Claude: cl}
	in := outcomes.Input{
		Records: recs,
		Worker: func(r route.Record) (outcomes.Transcript, bool) {
			return finder.Find(r.ClaudeSession, r.Name)
		},
		Session: func(id string) (outcomes.Transcript, bool) {
			t, ok, err := cl.IDTranscript(id)
			return t, ok && err == nil
		},
		PR: ghPR,
	}
	if *baseline {
		beforeT, err := baselineBefore(*before, all)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "qm report: %v\n", err)
			return 2
		}
		cfg, err := config.Load()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "qm report: %v\n", err)
			return 1
		}
		m, err := outcomes.NewMatcher(cfg.Report.WorkerMarkers)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "qm report: %v\n", err)
			return 2
		}
		in.Baseline = buildBaseline(cl, m, all, beforeT, projects)
		in.Baseline.Write(stdout)
		_, _ = fmt.Fprintln(stdout)
	}
	outcomes.Build(in).Write(stdout)
	return 0
}

// baselineBefore is the --before day, else the day of the first decision record.
func baselineBefore(flagVal string, all []route.Record) (time.Time, error) {
	if flagVal != "" {
		t, err := time.ParseInLocation(dateLayout, flagVal, time.Local)
		if err != nil {
			return t, fmt.Errorf("--before: %w", err)
		}
		return t, nil
	}
	if len(all) == 0 {
		return time.Time{}, fmt.Errorf("--baseline needs --before: the decision log is empty")
	}
	first := all[0].Time.Local()
	for _, r := range all {
		if r.Time.Before(all[0].Time) {
			first = r.Time.Local()
		}
	}
	return time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.Local), nil
}

// worktreeSuffix cuts a worktree path back to its repository folder.
var worktreeSuffix = regexp.MustCompile(`/\.claude/worktrees/.*$`)

// branchKey identifies a branch across a repository and its worktrees; "" for
// a transcript with no feature branch.
func branchKey(t outcomes.Transcript) string {
	switch t.Branch {
	case "", "main", "master", "HEAD":
		return ""
	}
	return worktreeSuffix.ReplaceAllString(t.Cwd, "") + "\x00" + t.Branch
}

// buildBaseline reads the worker transcripts that started before the cutoff,
// by the first-prompt markers of the 2026-11-19 analysis, from the projects
// whose folder name contains one of projects (all when empty). Sessions named
// by a decision record's Session (coordinators) never count.
func buildBaseline(cl outcomes.Claude, m outcomes.Matcher, all []route.Record, before time.Time, projects []string) *outcomes.Baseline {
	names := cl.NamesByID()
	typeByName, typeByID := map[string]string{}, map[string]string{}
	coordinators := map[string]bool{}
	for _, r := range all {
		if r.Kind == "worker" && r.Name != "" {
			typeByName[r.Name] = r.Type
		}
		if r.Kind == "worker" && r.ClaudeSession != "" {
			typeByID[r.ClaudeSession] = r.Type
		}
		if r.Session != "" {
			coordinators[r.Session] = true
		}
	}
	type entry struct {
		t        outcomes.Transcript
		typ      string
		reviewer bool
	}
	var entries []entry
	reviewers := map[string]int{}
	paths, _ := cl.AllTranscripts()
	for _, p := range paths {
		folder := filepath.Base(filepath.Dir(p))
		if !matchesProject(folder, projects) {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if coordinators[id] {
			continue
		}
		t, err := outcomes.ReadTranscript(p)
		if err != nil {
			continue
		}
		start, err := time.Parse(time.RFC3339, t.Start)
		if err != nil || !start.Before(before) || !m.IsWorker(t) {
			continue
		}
		typ, ok := typeByID[id]
		if !ok {
			typ = typeByName[names[id]]
		}
		e := entry{t: t, typ: typ, reviewer: m.IsReviewer(t)}
		if e.typ == "" {
			e.typ = outcomes.BriefType(t)
		}
		if e.reviewer {
			if k := branchKey(t); k != "" {
				reviewers[k]++
			}
		}
		entries = append(entries, e)
	}
	sessions := make([]outcomes.BaselineSession, 0, len(entries))
	for _, e := range entries {
		rounds := -1 // reviewers and sessions off a feature branch have no rounds
		if k := branchKey(e.t); k != "" && !e.reviewer {
			rounds = reviewers[k]
		}
		sessions = append(sessions, outcomes.BaselineSession{Type: e.typ, Rounds: rounds, Transcript: e.t})
	}
	return outcomes.ComputeBaseline(outcomes.BaselineInput{Records: all, Before: before, Sessions: sessions})
}

func matchesProject(folder string, projects []string) bool {
	if len(projects) == 0 {
		return true
	}
	for _, p := range projects {
		if strings.Contains(folder, p) {
			return true
		}
	}
	return false
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// ghPR asks gh for a pull request's size and state.
func ghPR(url string) (outcomes.PR, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", url, "--json", "additions,deletions,changedFiles,state").Output()
	if err != nil {
		return outcomes.PR{}, false
	}
	return parsePR(out)
}

// parsePR reads the JSON of gh pr view --json additions,deletions,changedFiles,state.
func parsePR(out []byte) (outcomes.PR, bool) {
	var v struct {
		Additions, Deletions, ChangedFiles int
		State                              string
	}
	if json.Unmarshal(out, &v) != nil {
		return outcomes.PR{}, false
	}
	return outcomes.PR{Additions: v.Additions, Deletions: v.Deletions, Files: v.ChangedFiles, Merged: v.State == "MERGED"}, true
}
