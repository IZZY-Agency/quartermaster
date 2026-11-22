package route

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// WorkerInput is what the coordinator gives route for one worker. Type is one
// of feature, fix, fix-pass, docs, release-paperwork, research, migration, ui,
// review; Size is S, M or L. Hint is the coordinator's own pick; Force, with
// ForceWhy, overrides the table.
type WorkerInput struct {
	Brief    string
	Type     string
	Size     string
	Repo     string
	Hint     *Tier
	Force    *Tier
	ForceWhy string
}

// WorkerJudgments are Jev's answers about a worker brief, and whether they
// could be had at all. Local: the privacy gate kept the brief on this
// machine. Failed: the gate or Jev failed. Either sends the table to the
// coordinator's hint.
type WorkerJudgments struct {
	Role       string
	Mechanical float64
	Security   float64
	LiveData   float64
	RootCause  float64
	DesignOpen float64
	Visual     float64
	Local      bool
	Failed     bool
}

// Reasons route gives when it cannot or does not ask Jev.
const (
	ReasonJevOff    = "Jev off (no TypeSafe key)"
	ReasonDownshift = "Claude 5-hour usage above 90%"
)

var (
	validModels  = []string{"sonnet", "opus", "fable", "haiku"}
	validEfforts = []string{"low", "medium", "high", "xhigh", "max"}
)

// DecideWorker picks the tier for a worker. ask nil means Jev is off: the gate
// is not called and nothing leaves the machine. A failing gate or Jev falls
// back to the hint (row local-error); only bad input is an error.
func DecideWorker(ctx context.Context, in WorkerInput, ask Asker, gate Gater, claude limits.ClaudeState) (Decision, error) {
	if err := checkWorkerInput(in); err != nil {
		return Decision{}, err
	}
	d := Decision{Kind: "worker"}
	if in.Force != nil {
		d.Tier, d.Row = *in.Force, "force"
		d.Reasons = []string{"forced by the coordinator: " + in.ForceWhy}
		return d, nil
	}

	var j WorkerJudgments
	switch {
	case ask == nil:
		j = WorkerJudgmentsWithoutJev(in)
		d.JevOff = true
		d.Reasons = append(d.Reasons, ReasonJevOff)
	case gate == nil:
		return Decision{}, errors.New("route: Jev is on but there is no privacy gate")
	default:
		res, err := gate.Gate(ctx, in.Brief)
		switch {
		case err != nil:
			j.Failed = true
			d.Reasons = append(d.Reasons, "privacy gate failed: "+err.Error())
		case res.Local:
			j.Local = true
			d.Local = true
			d.Reasons = append(d.Reasons, "privacy gate kept the brief local: "+res.Reason)
		default:
			j, err = askWorker(ctx, ask, res.Text)
			if err != nil {
				j = WorkerJudgments{Failed: true}
				d.Reasons = append(d.Reasons, "Jev failed: "+err.Error())
			}
		}
	}

	d.Tier, d.Row = WorkerTable(j, in)
	d.Reasons = append(d.Reasons, rowReason(d.Row, in))
	if ask != nil && !j.Local && !j.Failed {
		d.Judgments = judgmentMap(j)
	}
	if claude.Known && claude.FiveHourUsed > policy.DownshiftAbove && !workerRisk(j, in) &&
		!slices.Contains(policy.NoDownshiftRows, d.Row) {
		if lower, ok := downshift(d.Tier); ok {
			d.Tier = lower
			d.Reasons = append(d.Reasons, ReasonDownshift)
		}
	}
	return d, nil
}

// WorkerTable is the worker decision table: the first row that matches wins.
// It returns the tier and the row's name.
func WorkerTable(j WorkerJudgments, in WorkerInput) (Tier, string) {
	if in.Force != nil {
		return *in.Force, "force"
	}
	if j.Local || j.Failed {
		row := "local"
		if j.Failed {
			row = "local-error"
		}
		if in.Hint == nil {
			return Tier(policy.WorkerTiers["local"]), row
		}
		return capTier(*in.Hint), row
	}
	risk := workerRisk(j, in)
	designOpen := j.DesignOpen >= policy.YesAt
	// Either signal makes a research worker: Jev's role, or --type research.
	research := j.Role == "research" || in.Type == "research"
	switch {
	case research && designOpen:
		return Tier(policy.WorkerTiers["research-open"]), "research"
	case research:
		return Tier(policy.WorkerTiers["research"]), "research"
	case risk && (in.Size == "L" || designOpen):
		return Tier(policy.WorkerTiers["risk-big"]), "risk-big"
	case risk:
		return Tier(policy.WorkerTiers["risk"]), "risk"
	case in.Type == "ui" || j.Visual >= policy.YesAt:
		return Tier(policy.WorkerTiers["ui"]), "ui"
	case in.Size == "S" && (in.Type == "docs" || in.Type == "release-paperwork" || j.Mechanical >= policy.YesAt):
		return Tier(policy.WorkerTiers["s-light"]), "s-light"
	case in.Size == "S":
		return Tier(policy.WorkerTiers["s"]), "s"
	case designOpen || j.RootCause >= policy.YesAt:
		return Tier(policy.WorkerTiers["m-open"]), "m-open"
	case in.Size == "L":
		return Tier(policy.WorkerTiers["l"]), "l"
	default:
		return Tier(policy.WorkerTiers["m"]), "m"
	}
}

// WorkerJudgmentsWithoutJev stands in for Jev when there is no key: every
// Noul absent (0, not uncertain), except what Type and Hint say. docs and
// release-paperwork are mechanical, research is the research role, ui is
// visual, and on size M or L a hint of opus/medium or above leaves design
// open.
func WorkerJudgmentsWithoutJev(in WorkerInput) WorkerJudgments {
	var j WorkerJudgments
	switch in.Type {
	case "docs", "release-paperwork":
		j.Mechanical = 1
	case "research":
		j.Role = "research"
	case "ui":
		j.Visual = 1
	}
	if (in.Size == "M" || in.Size == "L") && in.Hint != nil &&
		(in.Hint.Model == "opus" || in.Hint.Model == "fable") &&
		slices.Index(validEfforts, in.Hint.Effort) >= slices.Index(validEfforts, "medium") {
		j.DesignOpen = 1
	}
	return j
}

// workerRisk is the table's Risk: the code signal, or security or live data
// not clearly "no" (uncertain counts as risk).
func workerRisk(j WorkerJudgments, in WorkerInput) bool {
	return workerCodeRisk(in) || j.Security > policy.NoAt || j.LiveData > policy.NoAt
}

// askWorker asks Jev the worker questions about the redacted brief, one
// request per section when it is long, and combines the sections on the safe
// side: the highest Noul, the lowest for mechanical, and the role answered
// with the highest probability.
func askWorker(ctx context.Context, ask Asker, brief string) (WorkerJudgments, error) {
	qs := make(map[string]typesafe.Question, len(policy.WorkerQuestions))
	for id, q := range policy.WorkerQuestions {
		qs[id] = typesafe.Question(q)
	}
	states, err := SplitState(policy.WorkerStateField, brief, qs)
	if err != nil {
		return WorkerJudgments{}, err
	}
	var j WorkerJudgments
	roleP := -1.0
	for i, state := range states {
		a, err := ask.Ask(ctx, state, qs)
		if err != nil {
			return WorkerJudgments{}, err
		}
		if p := a["role"].Probabilities[a["role"].Choice]; p > roleP {
			j.Role, roleP = a["role"].Choice, p
		}
		if i == 0 {
			j.Mechanical = a["mechanical"].Noul
		}
		j.Mechanical = min(j.Mechanical, a["mechanical"].Noul)
		j.Security = max(j.Security, a["security"].Noul)
		j.LiveData = max(j.LiveData, a["live_data"].Noul)
		j.RootCause = max(j.RootCause, a["root_cause"].Noul)
		j.DesignOpen = max(j.DesignOpen, a["design_open"].Noul)
		j.Visual = max(j.Visual, a["visual"].Noul)
	}
	return j, nil
}

// capTier lowers a hint to policy.LocalCap: fable becomes opus, and an effort
// above the cap's becomes the cap's.
func capTier(t Tier) Tier {
	limit := Tier(policy.LocalCap)
	if t.Model == "fable" {
		t.Model = limit.Model
	}
	if slices.Index(validEfforts, t.Effort) > slices.Index(validEfforts, limit.Effort) {
		t.Effort = limit.Effort
	}
	return t
}

// downshift returns the tier one step down policy.DownshiftLadder; false
// when t is not on it or is its last step.
func downshift(t Tier) (Tier, bool) {
	i := slices.Index(policy.DownshiftLadder, policy.Tier(t))
	if i < 0 || i == len(policy.DownshiftLadder)-1 {
		return t, false
	}
	return Tier(policy.DownshiftLadder[i+1]), true
}

func checkWorkerInput(in WorkerInput) error {
	if in.Size != "S" && in.Size != "M" && in.Size != "L" {
		return fmt.Errorf("route: size %q is not S, M or L", in.Size)
	}
	if in.Force != nil && strings.TrimSpace(in.ForceWhy) == "" {
		return errors.New("route: a forced tier needs a reason (--why)")
	}
	for _, f := range []struct {
		name string
		t    *Tier
	}{{"hint", in.Hint}, {"force", in.Force}} {
		name, t := f.name, f.t
		if t != nil && (!slices.Contains(validModels, t.Model) || !slices.Contains(validEfforts, t.Effort)) {
			return fmt.Errorf("route: %s %s/%s is not a model/effort (models %v, efforts %v)", name, t.Model, t.Effort, validModels, validEfforts)
		}
	}
	return nil
}

// rowReason says why a row matched, for the decision's reasons.
func rowReason(row string, in WorkerInput) string {
	switch row {
	case "local", "local-error":
		if in.Hint == nil {
			return "row " + row + ": no hint, opus/high"
		}
		return fmt.Sprintf("row %s: the coordinator's hint %s/%s, capped at opus/high", row, in.Hint.Model, in.Hint.Effort)
	case "risk", "risk-big":
		if workerCodeRisk(in) {
			return "row " + row + ": the brief or its type names a risky area"
		}
		return "row " + row + ": Jev sees security or live-data risk"
	}
	return fmt.Sprintf("row %s: type %s, size %s", row, in.Type, in.Size)
}

func judgmentMap(j WorkerJudgments) map[string]any {
	m := map[string]any{
		"mechanical": j.Mechanical, "security": j.Security, "live_data": j.LiveData,
		"root_cause": j.RootCause, "design_open": j.DesignOpen, "visual": j.Visual,
	}
	if j.Role != "" {
		m["role"] = j.Role
	}
	return m
}
