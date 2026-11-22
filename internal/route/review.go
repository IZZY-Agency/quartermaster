package route

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// ReviewInput is one review to route. Round 1 is the first review, 2 and up a
// re-check of named findings; DiffLines and DiffFiles are the size of the
// diff and Paths its changed files, computed by code. Force and ForceCodex
// override the Claude and Codex tiers.
type ReviewInput struct {
	Prompt     string
	Round      int
	DiffLines  int
	DiffFiles  int
	Paths      []string
	Repo       string
	Force      *Tier
	ForceCodex *CodexTier
}

// DecideReview picks the review tiers (design 5.4). Decision.Tier is always
// the Claude fallback; Decision.Codex is set only while Codex is available.
// ask nil means Jev is off: risk is the code signal only and the gate is not
// called. Force skips the Claude downshift. A failing gate or Jev, or a prompt the gate keeps local, also
// leaves the code signal only (preflight F10). Only bad input is an error.
func DecideReview(ctx context.Context, in ReviewInput, ask Asker, gate Gater, codex limits.CodexState, claude limits.ClaudeState) (Decision, error) {
	if err := checkReviewInput(in); err != nil {
		return Decision{}, err
	}
	d := Decision{Kind: "review"}
	codeRisk := CodeRisk(in.Prompt, in.Paths)
	risk := codeRisk
	if in.Round < 2 {
		switch {
		case ask == nil:
			d.JevOff = true
			d.Reasons = append(d.Reasons, ReasonJevOff)
		case gate == nil:
			return Decision{}, errors.New("route: Jev is on but there is no privacy gate")
		default:
			res, err := gate.Gate(ctx, in.Prompt)
			switch {
			case err != nil:
				d.Reasons = append(d.Reasons, "privacy gate failed: "+err.Error())
			case res.Local:
				d.Local = true
				d.Reasons = append(d.Reasons, "privacy gate kept the prompt local: "+res.Reason)
			default:
				security, dataRisk, err := askReview(ctx, ask, res.Text)
				if err != nil {
					d.Reasons = append(d.Reasons, "Jev failed: "+err.Error())
					break
				}
				d.Judgments = map[string]any{"security": security, "data_risk": dataRisk}
				risk = risk || security > policy.NoAt || dataRisk > policy.NoAt
			}
		}
	}

	d.Row = reviewRow(in, risk)
	row := policy.ReviewRows[d.Row]
	d.Tier = Tier(row.Claude)
	d.Reasons = append(d.Reasons, reviewRowReason(d.Row, in, codeRisk))
	if in.Force == nil && claude.Known && claude.FiveHourUsed > policy.DownshiftAbove && !slices.Contains(policy.ReviewRiskRows, d.Row) {
		if lower, ok := downshift(d.Tier); ok {
			d.Tier = lower
			d.Reasons = append(d.Reasons, ReasonDownshift)
		}
	}
	if codex.Available {
		d.Codex = &CodexTier{Model: row.CodexModel, Effort: row.CodexEffort}
	} else {
		why := codex.Reason
		if why == "" {
			why = "unavailable"
		}
		d.Reasons = append(d.Reasons, "Codex off ("+why+"): the review goes to Claude")
	}

	if in.Force != nil || in.ForceCodex != nil {
		d.Row = "force"
		if in.Force != nil {
			d.Tier = *in.Force
			d.Reasons = append(d.Reasons, fmt.Sprintf("Claude tier forced: %s/%s", in.Force.Model, in.Force.Effort))
		}
		switch {
		case in.ForceCodex != nil && d.Codex != nil:
			d.Codex = in.ForceCodex
			d.Reasons = append(d.Reasons, fmt.Sprintf("Codex tier forced: %s/%s", in.ForceCodex.Model, in.ForceCodex.Effort))
		case in.ForceCodex != nil:
			d.Reasons = append(d.Reasons, fmt.Sprintf("Codex tier forced to %s/%s, ignored: Codex is off", in.ForceCodex.Model, in.ForceCodex.Effort))
		}
	}
	return d, nil
}

// reviewRow is the review table's row: the first that matches wins.
func reviewRow(in ReviewInput, risk bool) string {
	switch {
	case in.Round >= 2:
		return "fix-check"
	case risk && in.DiffLines > policy.RiskLargeDiffOver:
		return "risk-large"
	case risk:
		return "risk"
	case in.DiffLines < policy.SmallDiffUnder:
		return "small"
	default:
		return "large"
	}
}

// askReview asks Jev the review questions about the redacted prompt, one
// request per section when it is long, and keeps the highest answer of each.
func askReview(ctx context.Context, ask Asker, prompt string) (security, dataRisk float64, err error) {
	qs := make(map[string]typesafe.Question, len(policy.ReviewQuestions))
	for id, q := range policy.ReviewQuestions {
		qs[id] = typesafe.Question(q)
	}
	states, err := SplitState(policy.ReviewStateField, prompt, qs)
	if err != nil {
		return 0, 0, err
	}
	for _, state := range states {
		a, err := ask.Ask(ctx, state, qs)
		if err != nil {
			return 0, 0, err
		}
		security = max(security, a["security"].Noul)
		dataRisk = max(dataRisk, a["data_risk"].Noul)
	}
	return security, dataRisk, nil
}

func checkReviewInput(in ReviewInput) error {
	if in.Round < 0 || in.DiffLines < 0 || in.DiffFiles < 0 {
		return fmt.Errorf("route: round %d, diff lines %d and diff files %d cannot be negative", in.Round, in.DiffLines, in.DiffFiles)
	}
	if t := in.Force; t != nil && (!slices.Contains(validModels, t.Model) || !slices.Contains(validEfforts, t.Effort)) {
		return fmt.Errorf("route: force %s/%s is not a model/effort (models %v, efforts %v)", t.Model, t.Effort, validModels, validEfforts)
	}
	if t := in.ForceCodex; t != nil && (!slices.Contains(policy.CodexModels, t.Model) || !slices.Contains(policy.CodexEfforts, t.Effort)) {
		return fmt.Errorf("route: Codex force %s/%s is not a Codex model/effort (models %v, efforts %v)", t.Model, t.Effort, policy.CodexModels, policy.CodexEfforts)
	}
	return nil
}

// reviewRowReason says why a review row matched, for the decision's reasons.
func reviewRowReason(row string, in ReviewInput, codeRisk bool) string {
	switch row {
	case "fix-check":
		return fmt.Sprintf("row fix-check: round %d re-checks named findings", in.Round)
	case "risk", "risk-large":
		why := "Jev sees security or data risk"
		if codeRisk {
			why = "the prompt or a changed path names a risky area"
		}
		return fmt.Sprintf("row %s: %s, diff %d lines in %d files", row, why, in.DiffLines, in.DiffFiles)
	}
	return fmt.Sprintf("row %s: diff %d lines in %d files", row, in.DiffLines, in.DiffFiles)
}
