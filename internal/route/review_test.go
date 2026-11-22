package route

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/privacy"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

var codexOn = limits.CodexState{Available: true, UsedPercent: 20}

// reviewJev answers every review question: security and data_risk as given.
func reviewJev(security, dataRisk float64) *fakeAsker {
	return &fakeAsker{answer: func(int) (map[string]typesafe.Answer, error) {
		return reviewAnswers(security, dataRisk), nil
	}}
}

func reviewAnswers(security, dataRisk float64) map[string]typesafe.Answer {
	return map[string]typesafe.Answer{
		"security":  {Type: "noul", Noul: security},
		"data_risk": {Type: "noul", Noul: dataRisk},
	}
}

func codexTier(model, effort string) *CodexTier { return &CodexTier{Model: model, Effort: effort} }

func TestReviewRows(t *testing.T) {
	for _, c := range []struct {
		name   string
		in     ReviewInput
		ask    *fakeAsker
		row    string
		codex  CodexTier
		claude Tier
	}{
		{"fix-check", ReviewInput{Prompt: "check the fixes", Round: 2, DiffLines: 5000}, reviewJev(0.9, 0.9), "fix-check", CodexTier{"gpt-6.2-sol", "medium"}, Tier{"sonnet", "medium"}},
		{"small", ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 299}, reviewJev(0.1, 0.2), "small", CodexTier{"gpt-6.2-sol", "medium"}, Tier{"sonnet", "high"}},
		{"large", ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 300}, reviewJev(0.1, 0.3), "large", CodexTier{"gpt-6.2-sol", "high"}, Tier{"opus", "medium"}},
		{"risk", ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 1000}, reviewJev(0.45, 0), "risk", CodexTier{"gpt-6.2-sol", "xhigh"}, Tier{"opus", "high"}},
		{"risk from data_risk", ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 20}, reviewJev(0, 0.31), "risk", CodexTier{"gpt-6.2-sol", "xhigh"}, Tier{"opus", "high"}},
		{"risk from a path", ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 20, Paths: []string{"internal/auth/session.go"}}, reviewJev(0, 0), "risk", CodexTier{"gpt-6.2-sol", "xhigh"}, Tier{"opus", "high"}},
		{"risk-large", ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 1001}, reviewJev(0.8, 0), "risk-large", CodexTier{"gpt-6.2-astra", "xhigh"}, Tier{"opus", "xhigh"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := DecideReview(context.Background(), c.in, c.ask, &fakeGate{}, codexOn, noLimits)
			if err != nil {
				t.Fatal(err)
			}
			if d.Kind != "review" || d.Row != c.row || d.Tier != c.claude || d.Codex == nil || *d.Codex != c.codex {
				t.Errorf("got kind %s row %s claude %v codex %v; want %s %v %v", d.Kind, d.Row, d.Tier, d.Codex, c.row, c.claude, c.codex)
			}
		})
	}
}

func TestReviewReasonsNameDiffSize(t *testing.T) {
	for _, c := range []struct {
		ask  *fakeAsker
		want string
	}{
		{reviewJev(0, 0), "row small: diff 40 lines in 3 files"},
		{reviewJev(0.9, 0), "diff 40 lines in 3 files"},
	} {
		d, err := DecideReview(context.Background(), ReviewInput{Prompt: "review", Round: 1, DiffLines: 40, DiffFiles: 3}, c.ask, &fakeGate{}, codexOn, noLimits)
		if err != nil {
			t.Fatal(err)
		}
		if !hasReason(d, c.want) {
			t.Errorf("row %s: reasons %v, want %q", d.Row, d.Reasons, c.want)
		}
	}
}

func TestFixCheckAsksNothing(t *testing.T) {
	ask, gate := reviewJev(0.9, 0.9), &fakeGate{}
	if _, err := DecideReview(context.Background(), ReviewInput{Prompt: "check", Round: 3}, ask, gate, codexOn, noLimits); err != nil {
		t.Fatal(err)
	}
	if len(ask.states) != 0 || gate.calls != 0 {
		t.Errorf("Jev asked %d times, gate called %d times on a fix check", len(ask.states), gate.calls)
	}
}

func TestReviewSendsRedactedPrompt(t *testing.T) {
	ask := reviewJev(0, 0)
	gate := &fakeGate{res: &privacy.Result{Text: "review <EMAIL_1>'s change"}}
	d, err := DecideReview(context.Background(), ReviewInput{Prompt: "review <email>'s change", Round: 1, DiffLines: 10}, ask, gate, codexOn, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(ask.states) != 1 {
		t.Fatalf("%d Jev requests", len(ask.states))
	}
	state := ask.states[0].(map[string]any)
	if state[policy.ReviewStateField] != "review <EMAIL_1>'s change" {
		t.Errorf("state = %v", state)
	}
	if _, ok := ask.qs[0]["security"]; !ok || len(ask.qs[0]) != len(policy.ReviewQuestions) {
		t.Errorf("questions = %v", ask.qs[0])
	}
	if d.Judgments["security"] != 0.0 || d.Judgments["data_risk"] != 0.0 {
		t.Errorf("judgments = %v", d.Judgments)
	}
}

func TestReviewSplitsLongPrompt(t *testing.T) {
	var b strings.Builder
	for b.Len() < 160000 {
		b.WriteString("Review the diff below. \"quoted\" <a> & <b>: the helper and its callers changed.\n")
	}
	prompt := b.String()[:160000]
	ask := &fakeAsker{answer: func(n int) (map[string]typesafe.Answer, error) {
		if n == 1 {
			return reviewAnswers(0.2, 0.8), nil // one risky section is enough
		}
		return reviewAnswers(0.1, 0), nil
	}}
	d, err := DecideReview(context.Background(), ReviewInput{Prompt: prompt, Round: 1, DiffLines: 120}, ask, &fakeGate{}, codexOn, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(ask.states) < 2 {
		t.Fatalf("%d Jev requests for a 160,000-character prompt, want it split", len(ask.states))
	}
	var joined strings.Builder
	for i, s := range ask.states {
		state := s.(map[string]any)
		joined.WriteString(state[policy.ReviewStateField].(string))
		req := map[string]any{"model": policy.Model, "state": state, "questions": ask.qs[i]}
		if tok := requestBytes(t, req) / policy.CharsPerToken; tok >= policy.JevRequestTokens {
			t.Errorf("request %d: %d tokens", i, tok)
		}
		for id, q := range ask.qs[i] {
			if tok := (requestBytes(t, state) + requestBytes(t, q)) / policy.CharsPerToken; tok >= policy.JevQuestionTokens {
				t.Errorf("request %d, question %s: %d tokens", i, id, tok)
			}
		}
	}
	if joined.String() != prompt {
		t.Error("the sections do not add up to the prompt")
	}
	if d.Row != "risk" || d.Judgments["data_risk"] != 0.8 || d.Judgments["security"] != 0.2 {
		t.Errorf("row %s, judgments %v: want the sections combined by max", d.Row, d.Judgments)
	}
}

func TestNoCodexReviewsGoToClaude(t *testing.T) {
	st, err := limits.Codex(func(string) (string, error) { return "", errors.New("not found") })
	if err != nil {
		t.Fatal(err)
	}
	if st.Available || st.Reason != "codex not installed" {
		t.Fatalf("codex state = %+v", st)
	}
	for _, c := range []struct {
		in     ReviewInput
		ask    *fakeAsker
		row    string
		claude Tier
	}{
		{ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 120}, reviewJev(0, 0), "small", Tier{"sonnet", "high"}},
		{ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 2000}, reviewJev(0.9, 0), "risk-large", Tier{"opus", "xhigh"}},
		{ReviewInput{Prompt: "check the fixes", Round: 2}, reviewJev(0, 0), "fix-check", Tier{"sonnet", "medium"}},
	} {
		d, err := DecideReview(context.Background(), c.in, c.ask, &fakeGate{}, st, noLimits)
		if err != nil {
			t.Fatal(err)
		}
		if d.Codex != nil || d.Row != c.row || d.Tier != c.claude {
			t.Errorf("%s: codex %v row %s claude %v", c.row, d.Codex, d.Row, d.Tier)
		}
		if !hasReason(d, "codex not installed") {
			t.Errorf("%s: reasons %v do not say why Codex is off", c.row, d.Reasons)
		}
	}
}

func TestReviewWithoutJev(t *testing.T) {
	gate := &fakeGate{}
	d, err := DecideReview(context.Background(), ReviewInput{Prompt: "Review the new OAuth callback handler.", Round: 1, DiffLines: 120}, nil, gate, codexOn, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if d.Row != "risk" || !d.JevOff || !hasReason(d, ReasonJevOff) {
		t.Errorf("auth prompt: row %s, jev off %v, reasons %v", d.Row, d.JevOff, d.Reasons)
	}
	d, err = DecideReview(context.Background(), ReviewInput{Prompt: "Review the README wording change.", Round: 1, DiffLines: 120}, nil, gate, codexOn, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if d.Row != "small" || d.Judgments != nil {
		t.Errorf("plain prompt: row %s, judgments %v", d.Row, d.Judgments)
	}
	if gate.calls != 0 {
		t.Errorf("gate called %d times with Jev off", gate.calls)
	}
}

// A failing gate or Jev, or a gate that keeps the prompt local, leaves the
// code signal as the only risk signal (preflight F10).
func TestReviewFallsBackToCodeRisk(t *testing.T) {
	failing := &fakeAsker{answer: func(int) (map[string]typesafe.Answer, error) { return nil, errors.New("jev down") }}
	for _, c := range []struct {
		name   string
		ask    *fakeAsker
		gate   *fakeGate
		reason string
		local  bool
	}{
		{"gate error", reviewJev(0.9, 0.9), &fakeGate{err: errors.New("helper crashed")}, "privacy gate failed", false},
		{"jev error", failing, &fakeGate{}, "Jev failed", false},
		{"local", reviewJev(0.9, 0.9), &fakeGate{res: &privacy.Result{Local: true, Reason: "leftover"}}, "kept the prompt local", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			plain, err := DecideReview(context.Background(), ReviewInput{Prompt: "review the change", Round: 1, DiffLines: 50}, c.ask, c.gate, codexOn, noLimits)
			if err != nil {
				t.Fatal(err)
			}
			if plain.Row != "small" || plain.Local != c.local || !hasReason(plain, c.reason) || plain.Judgments != nil {
				t.Errorf("plain: row %s local %v reasons %v judgments %v", plain.Row, plain.Local, plain.Reasons, plain.Judgments)
			}
			risky, err := DecideReview(context.Background(), ReviewInput{Prompt: "review the payment flow", Round: 1, DiffLines: 50}, c.ask, c.gate, codexOn, noLimits)
			if err != nil {
				t.Fatal(err)
			}
			if risky.Row != "risk" {
				t.Errorf("payment prompt: row %s", risky.Row)
			}
		})
	}
	if _, err := DecideReview(context.Background(), ReviewInput{Prompt: "x", Round: 1}, reviewJev(0, 0), nil, codexOn, noLimits); err == nil {
		t.Error("Jev on without a gate: no error")
	}
}

func TestReviewDownshift(t *testing.T) {
	busy := limits.ClaudeState{Known: true, FiveHourUsed: 95}
	for _, c := range []struct {
		in     ReviewInput
		ask    *fakeAsker
		claude Tier
		shift  bool
	}{
		{ReviewInput{Prompt: "review", Round: 1, DiffLines: 50}, reviewJev(0, 0), Tier{"sonnet", "medium"}, true},   // small: sonnet/high
		{ReviewInput{Prompt: "review", Round: 1, DiffLines: 500}, reviewJev(0, 0), Tier{"sonnet", "high"}, true},    // large: opus/medium
		{ReviewInput{Prompt: "check", Round: 2}, reviewJev(0, 0), Tier{"sonnet", "low"}, true},                      // fix-check: sonnet/medium
		{ReviewInput{Prompt: "review", Round: 1, DiffLines: 50}, reviewJev(0.5, 0), Tier{"opus", "high"}, false},    // risk
		{ReviewInput{Prompt: "review", Round: 1, DiffLines: 5000}, reviewJev(0.5, 0), Tier{"opus", "xhigh"}, false}, // risk-large
	} {
		d, err := DecideReview(context.Background(), c.in, c.ask, &fakeGate{}, codexOn, busy)
		if err != nil {
			t.Fatal(err)
		}
		if d.Tier != c.claude || hasReason(d, ReasonDownshift) != c.shift {
			t.Errorf("row %s: claude %v, reasons %v; want %v, downshift %v", d.Row, d.Tier, d.Reasons, c.claude, c.shift)
		}
		if d.Codex == nil {
			t.Errorf("row %s: the downshift dropped Codex", d.Row)
		}
	}
	d, err := DecideReview(context.Background(), ReviewInput{Prompt: "review", Round: 1, DiffLines: 50}, reviewJev(0, 0), &fakeGate{},
		codexOn, limits.ClaudeState{Known: false, FiveHourUsed: 99})
	if err != nil {
		t.Fatal(err)
	}
	if d.Tier != (Tier{"sonnet", "high"}) {
		t.Errorf("limits unknown: %v", d.Tier)
	}
}

func TestReviewForce(t *testing.T) {
	in := ReviewInput{Prompt: "review", Round: 1, DiffLines: 50, Force: tier("fable", "max"), ForceCodex: codexTier("gpt-6.2-astra", "high")}
	d, err := DecideReview(context.Background(), in, reviewJev(0, 0), &fakeGate{}, codexOn, limits.ClaudeState{Known: true, FiveHourUsed: 99})
	if err != nil {
		t.Fatal(err)
	}
	if d.Row != "force" || d.Tier != *in.Force || d.Codex == nil || *d.Codex != *in.ForceCodex {
		t.Errorf("got row %s claude %v codex %v", d.Row, d.Tier, d.Codex)
	}
	if hasReason(d, ReasonDownshift) {
		t.Errorf("forced tier, yet reasons %v name a downshift", d.Reasons)
	}
	// A forced Codex tier does not make Codex available.
	d, err = DecideReview(context.Background(), ReviewInput{Prompt: "review", Round: 1, DiffLines: 50, ForceCodex: codexTier("gpt-6.2-sol", "low")},
		reviewJev(0, 0), &fakeGate{}, limits.CodexState{Reason: "codex not installed"}, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if d.Codex != nil || d.Row != "force" || d.Tier != (Tier{"sonnet", "high"}) {
		t.Errorf("forced codex while off: row %s claude %v codex %v", d.Row, d.Tier, d.Codex)
	}
	if !hasReason(d, "Codex tier forced to gpt-6.2-sol/low, ignored: Codex is off") {
		t.Errorf("forced codex while off: reasons %v do not say the force was ignored", d.Reasons)
	}
}

func TestDecideReviewRejectsBadInput(t *testing.T) {
	for name, in := range map[string]ReviewInput{
		"negative round":       {Prompt: "x", Round: -1},
		"negative diff":        {Prompt: "x", Round: 1, DiffLines: -1},
		"bad force":            {Prompt: "x", Round: 1, Force: tier("gpt", "high")},
		"bad codex model":      {Prompt: "x", Round: 1, ForceCodex: codexTier("sonnet", "high")},
		"codex effort > xhigh": {Prompt: "x", Round: 1, ForceCodex: codexTier("gpt-6.2-sol", "max")},
	} {
		if _, err := DecideReview(context.Background(), in, nil, nil, codexOn, noLimits); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
