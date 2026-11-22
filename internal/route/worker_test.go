package route

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IZZY-Agency/quartermaster/internal/limits"
	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/privacy"
	"github.com/IZZY-Agency/quartermaster/internal/typesafe"
)

// fakeAsker answers every call with answer(n), n counting calls from 0, and
// records each call's state and questions.
type fakeAsker struct {
	answer func(n int) (map[string]typesafe.Answer, error)
	states []any
	qs     []map[string]typesafe.Question
}

func (f *fakeAsker) Ask(_ context.Context, state any, qs map[string]typesafe.Question) (map[string]typesafe.Answer, error) {
	n := len(f.states)
	f.states = append(f.states, state)
	f.qs = append(f.qs, qs)
	return f.answer(n)
}

// fakeGate passes the text through unchanged unless res or err is set, and
// counts its calls.
type fakeGate struct {
	res   *privacy.Result
	err   error
	calls int
}

func (g *fakeGate) Gate(_ context.Context, text string) (privacy.Result, error) {
	g.calls++
	if g.err != nil {
		return privacy.Result{}, g.err
	}
	if g.res != nil {
		return *g.res, nil
	}
	return privacy.Result{Text: text}, nil
}

// answers builds a Jev reply: every worker Noul from nouls (0 when absent),
// and role with probability p.
func answers(role string, p float64, nouls map[string]float64) map[string]typesafe.Answer {
	out := map[string]typesafe.Answer{
		"role": {Type: "choice", Choice: role, Probabilities: map[string]float64{role: p}},
	}
	for id, q := range policy.WorkerQuestions {
		if q.Type == "noul" {
			out[id] = typesafe.Answer{Type: "noul", Noul: nouls[id]}
		}
	}
	return out
}

func jev(role string, nouls map[string]float64) *fakeAsker {
	return &fakeAsker{answer: func(int) (map[string]typesafe.Answer, error) { return answers(role, 0.9, nouls), nil }}
}

func tier(model, effort string) *Tier { return &Tier{Model: model, Effort: effort} }

func hasReason(d Decision, part string) bool {
	for _, r := range d.Reasons {
		if strings.Contains(r, part) {
			return true
		}
	}
	return false
}

var noLimits = limits.ClaudeState{}

func TestWorkerRows(t *testing.T) {
	local := &privacy.Result{Local: true, Reason: privacy.ReasonStillPrivate}
	cases := []struct {
		name  string
		in    WorkerInput
		ask   *fakeAsker
		gate  *fakeGate
		want  Tier
		row   string
		local bool
	}{
		{"force", WorkerInput{Brief: "fix a typo", Size: "S", Type: "docs", Force: tier("fable", "max"), ForceWhy: "owner asked"}, jev("implement", nil), &fakeGate{}, Tier{"fable", "max"}, "force", false},
		{"local caps fable/xhigh", WorkerInput{Brief: "b", Size: "M", Hint: tier("fable", "xhigh")}, jev("implement", nil), &fakeGate{res: local}, Tier{"opus", "high"}, "local", true},
		{"local caps max", WorkerInput{Brief: "b", Size: "M", Hint: tier("sonnet", "max")}, jev("implement", nil), &fakeGate{res: local}, Tier{"sonnet", "high"}, "local", true},
		{"local keeps a lower hint", WorkerInput{Brief: "b", Size: "L", Hint: tier("sonnet", "medium")}, jev("implement", nil), &fakeGate{res: local}, Tier{"sonnet", "medium"}, "local", true},
		{"local without hint", WorkerInput{Brief: "b", Size: "S"}, jev("implement", nil), &fakeGate{res: local}, Tier{"opus", "high"}, "local", true},
		{"gate error", WorkerInput{Brief: "b", Size: "S", Hint: tier("sonnet", "low")}, jev("implement", nil), &fakeGate{err: errors.New("helper crashed")}, Tier{"sonnet", "low"}, "local-error", false},
		{"research", WorkerInput{Brief: "b", Size: "L", Type: "research"}, jev("research", nil), &fakeGate{}, Tier{"sonnet", "medium"}, "research", false},
		{"research design-open", WorkerInput{Brief: "b", Size: "S"}, jev("research", map[string]float64{"design_open": 0.7}), &fakeGate{}, Tier{"opus", "medium"}, "research", false},
		{"risk-big on size L", WorkerInput{Brief: "b", Size: "L"}, jev("implement", map[string]float64{"security": 0.8}), &fakeGate{}, Tier{"opus", "xhigh"}, "risk-big", false},
		{"risk-big on design-open", WorkerInput{Brief: "b", Size: "M"}, jev("implement", map[string]float64{"live_data": 0.5, "design_open": 0.9}), &fakeGate{}, Tier{"opus", "xhigh"}, "risk-big", false},
		{"risk on uncertain security", WorkerInput{Brief: "b", Size: "M"}, jev("implement", map[string]float64{"security": 0.45}), &fakeGate{}, Tier{"opus", "high"}, "risk", false},
		{"risk from the brief", WorkerInput{Brief: "Add a Stripe webhook handler.", Size: "S", Type: "docs"}, jev("implement", map[string]float64{"mechanical": 1}), &fakeGate{}, Tier{"opus", "high"}, "risk", false},
		{"risk from type migration", WorkerInput{Brief: "b", Size: "S", Type: "migration"}, jev("implement", nil), &fakeGate{}, Tier{"opus", "high"}, "risk", false},
		{"ui by type", WorkerInput{Brief: "b", Size: "S", Type: "ui"}, jev("implement", map[string]float64{"mechanical": 0.9}), &fakeGate{}, Tier{"opus", "medium"}, "ui", false},
		{"ui by visual", WorkerInput{Brief: "b", Size: "M"}, jev("implement", map[string]float64{"visual": 0.7}), &fakeGate{}, Tier{"opus", "medium"}, "ui", false},
		{"s-light by type", WorkerInput{Brief: "b", Size: "S", Type: "docs"}, jev("implement", nil), &fakeGate{}, Tier{"sonnet", "low"}, "s-light", false},
		{"s-light by mechanical", WorkerInput{Brief: "b", Size: "S", Type: "fix"}, jev("implement", map[string]float64{"mechanical": 0.7}), &fakeGate{}, Tier{"sonnet", "low"}, "s-light", false},
		{"s", WorkerInput{Brief: "b", Size: "S", Type: "fix"}, jev("implement", map[string]float64{"mechanical": 0.5}), &fakeGate{}, Tier{"sonnet", "medium"}, "s", false},
		{"m-open by root cause", WorkerInput{Brief: "b", Size: "M", Type: "fix"}, jev("implement", map[string]float64{"root_cause": 0.7}), &fakeGate{}, Tier{"opus", "medium"}, "m-open", false},
		{"m-open by design-open", WorkerInput{Brief: "b", Size: "L"}, jev("implement", map[string]float64{"design_open": 0.8}), &fakeGate{}, Tier{"opus", "medium"}, "m-open", false},
		{"l", WorkerInput{Brief: "b", Size: "L"}, jev("implement", nil), &fakeGate{}, Tier{"opus", "medium"}, "l", false},
		{"m", WorkerInput{Brief: "b", Size: "M", Type: "feature"}, jev("implement", map[string]float64{"design_open": 0.6}), &fakeGate{}, Tier{"sonnet", "high"}, "m", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := DecideWorker(context.Background(), c.in, c.ask, c.gate, noLimits)
			if err != nil {
				t.Fatal(err)
			}
			if d.Tier != c.want || d.Row != c.row || d.Local != c.local || d.Kind != "worker" {
				t.Errorf("got %+v row %q local %v kind %q, want %+v row %q local %v", d.Tier, d.Row, d.Local, d.Kind, c.want, c.row, c.local)
			}
			if d.JevOff {
				t.Error("JevOff set with an Asker")
			}
		})
	}
}

func TestForceSkipsGateAndJev(t *testing.T) {
	ask, gate := jev("implement", nil), &fakeGate{}
	d, err := DecideWorker(context.Background(), WorkerInput{Brief: "b", Size: "S", Force: tier("fable", "high"), ForceWhy: "owner asked"}, ask, gate, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if gate.calls != 0 || len(ask.states) != 0 {
		t.Errorf("gate calls %d, Jev calls %d; want none", gate.calls, len(ask.states))
	}
	if !hasReason(d, "owner asked") {
		t.Errorf("reasons %q lack the force reason", d.Reasons)
	}
}

func TestLocalSendsNothing(t *testing.T) {
	ask := jev("implement", nil)
	gate := &fakeGate{res: &privacy.Result{Local: true, Reason: privacy.ReasonStillPrivate}}
	d, err := DecideWorker(context.Background(), WorkerInput{Brief: "b", Size: "M", Hint: tier("opus", "medium")}, ask, gate, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(ask.states) != 0 {
		t.Errorf("Jev called %d times for a local brief", len(ask.states))
	}
	if d.Tier != (Tier{"opus", "medium"}) || !hasReason(d, privacy.ReasonStillPrivate) {
		t.Errorf("got %+v reasons %q", d.Tier, d.Reasons)
	}
}

func TestJevErrorFallsBackToHint(t *testing.T) {
	ask := &fakeAsker{answer: func(int) (map[string]typesafe.Answer, error) { return nil, errors.New("typesafe: HTTP 503") }}
	d, err := DecideWorker(context.Background(), WorkerInput{Brief: "b", Size: "S", Type: "docs", Hint: tier("sonnet", "high")}, ask, &fakeGate{}, noLimits)
	if err != nil {
		t.Fatalf("a Jev failure must fall back, not fail: %v", err)
	}
	if d.Tier != (Tier{"sonnet", "high"}) || d.Row != "local-error" {
		t.Errorf("got %+v row %q, want sonnet/high local-error", d.Tier, d.Row)
	}
	if !hasReason(d, "HTTP 503") {
		t.Errorf("reasons %q lack the Jev error", d.Reasons)
	}
	d, err = DecideWorker(context.Background(), WorkerInput{Brief: "b", Size: "S", Type: "docs", Hint: tier("fable", "max")}, ask, &fakeGate{}, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tier != (Tier{"opus", "high"}) {
		t.Errorf("fable/max hint on a Jev error gave %+v, want opus/high", d.Tier)
	}
}

func TestDecideWorkerRejectsBadInput(t *testing.T) {
	for _, in := range []WorkerInput{
		{Brief: "b", Size: "XL"},
		{Brief: "b", Size: ""},
		{Brief: "b", Size: "S", Hint: tier("gpt", "high")},
		{Brief: "b", Size: "S", Force: tier("opus", "huge"), ForceWhy: "x"},
		{Brief: "b", Size: "S", Force: tier("opus", "high")},
		{Brief: "b", Size: "S", Force: tier("opus", "high"), ForceWhy: "  "},
	} {
		if _, err := DecideWorker(context.Background(), in, nil, nil, noLimits); err == nil {
			t.Errorf("%+v: no error", in)
		}
	}
	if _, err := DecideWorker(context.Background(), WorkerInput{Brief: "b", Size: "S"}, jev("implement", nil), nil, noLimits); err == nil {
		t.Error("an Asker without a gate: no error")
	}
}

func TestNoFableWithoutForce(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	models := []string{"haiku", "sonnet", "opus", "fable"}
	efforts := []string{"low", "medium", "high", "xhigh", "max"}
	sizes := []string{"S", "M", "L"}
	types := []string{"feature", "fix", "fix-pass", "docs", "release-paperwork", "research", "migration", "ui", "review"}
	roles := []string{"implement", "review", "research"}
	for i := range 20000 {
		j := WorkerJudgments{
			Role: roles[r.IntN(len(roles))], Mechanical: r.Float64(), Security: r.Float64(), LiveData: r.Float64(),
			RootCause: r.Float64(), DesignOpen: r.Float64(), Visual: r.Float64(), Local: r.IntN(4) == 0, Failed: r.IntN(4) == 0,
		}
		in := WorkerInput{Brief: "b", Size: sizes[r.IntN(3)], Type: types[r.IntN(len(types))]}
		if r.IntN(3) > 0 {
			in.Hint = tier(models[r.IntN(4)], efforts[r.IntN(5)])
		}
		if got, row := WorkerTable(j, in); got.Model == "fable" {
			t.Fatalf("case %d: %+v %+v gave fable on row %q", i, j, in, row)
		}
		if got, row := WorkerTable(WorkerJudgmentsWithoutJev(in), in); got.Model == "fable" {
			t.Fatalf("case %d without Jev: %+v gave fable on row %q", i, in, row)
		}
	}
}

func TestDownshift(t *testing.T) {
	hot := limits.ClaudeState{Known: true, FiveHourUsed: 95}
	cases := []struct {
		name  string
		in    WorkerInput
		ask   *fakeAsker
		claud limits.ClaudeState
		want  Tier
		down  bool
	}{
		{"l steps down", WorkerInput{Brief: "b", Size: "L"}, jev("implement", nil), hot, Tier{"sonnet", "high"}, true},
		{"m steps down", WorkerInput{Brief: "b", Size: "M"}, jev("implement", nil), hot, Tier{"sonnet", "medium"}, true},
		{"s steps down", WorkerInput{Brief: "b", Size: "S"}, jev("implement", nil), hot, Tier{"sonnet", "low"}, true},
		{"bottom stays", WorkerInput{Brief: "b", Size: "S", Type: "docs"}, jev("implement", nil), hot, Tier{"sonnet", "low"}, false},
		{"at 90 no step", WorkerInput{Brief: "b", Size: "L"}, jev("implement", nil), limits.ClaudeState{Known: true, FiveHourUsed: 90}, Tier{"opus", "medium"}, false},
		{"risk keeps", WorkerInput{Brief: "b", Size: "M"}, jev("implement", map[string]float64{"security": 0.5}), hot, Tier{"opus", "high"}, false},
		{"code risk keeps", WorkerInput{Brief: "Run the database migration.", Size: "L"}, jev("implement", nil), hot, Tier{"opus", "xhigh"}, false},
		{"ui floor keeps", WorkerInput{Brief: "b", Size: "M", Type: "ui"}, jev("implement", nil), hot, Tier{"opus", "medium"}, false},
		{"force keeps", WorkerInput{Brief: "b", Size: "M", Force: tier("opus", "medium"), ForceWhy: "owner asked"}, jev("implement", nil), hot, Tier{"opus", "medium"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := DecideWorker(context.Background(), c.in, c.ask, &fakeGate{}, c.claud)
			if err != nil {
				t.Fatal(err)
			}
			if d.Tier != c.want {
				t.Errorf("got %+v, want %+v", d.Tier, c.want)
			}
			if got := hasReason(d, "Claude 5-hour usage above 90%"); got != c.down {
				t.Errorf("downshift reason %v, want %v (reasons %q)", got, c.down, d.Reasons)
			}
		})
	}
}

func TestNoDownshiftWhenLimitsUnknown(t *testing.T) {
	d, err := DecideWorker(context.Background(), WorkerInput{Brief: "b", Size: "L"}, jev("implement", nil), &fakeGate{}, limits.ClaudeState{FiveHourUsed: 99})
	if err != nil {
		t.Fatal(err)
	}
	if d.Tier != (Tier{"opus", "medium"}) || hasReason(d, "Claude 5-hour") {
		t.Errorf("got %+v reasons %q, want opus/medium with no downshift", d.Tier, d.Reasons)
	}
}

func TestNoKeyRoutesWithoutJev(t *testing.T) {
	cases := []struct {
		in   WorkerInput
		want Tier
		row  string
	}{
		{WorkerInput{Brief: "Fix the README links.", Size: "S", Type: "docs"}, Tier{"sonnet", "low"}, "s-light"},
		{WorkerInput{Brief: "Add a list view.", Size: "M", Hint: tier("sonnet", "high")}, Tier{"sonnet", "high"}, "m"},
		{WorkerInput{Brief: "Add a list view.", Size: "M", Hint: tier("opus", "high")}, Tier{"opus", "medium"}, "m-open"},
		{WorkerInput{Brief: "Write the migration for the orders table.", Size: "M", Type: "feature"}, Tier{"opus", "high"}, "risk"},
		{WorkerInput{Brief: "Compare two caching designs.", Size: "M", Type: "research", Hint: tier("opus", "medium")}, Tier{"opus", "medium"}, "research"},
		{WorkerInput{Brief: "Compare two caching designs.", Size: "S", Type: "research", Hint: tier("opus", "high")}, Tier{"sonnet", "medium"}, "research"},
		{WorkerInput{Brief: "Restyle the header.", Size: "S", Type: "ui"}, Tier{"opus", "medium"}, "ui"},
		{WorkerInput{Brief: "Fix the off-by-one.", Size: "S", Type: "fix"}, Tier{"sonnet", "medium"}, "s"},
	}
	for _, c := range cases {
		gate := &fakeGate{}
		d, err := DecideWorker(context.Background(), c.in, nil, gate, noLimits)
		if err != nil {
			t.Fatal(err)
		}
		if d.Tier != c.want || d.Row != c.row {
			t.Errorf("%+v: got %+v row %q, want %+v row %q", c.in, d.Tier, d.Row, c.want, c.row)
		}
		if gate.calls != 0 {
			t.Errorf("%+v: gate called %d times with Jev off", c.in, gate.calls)
		}
		if !d.JevOff || !hasReason(d, "Jev off (no TypeSafe key)") {
			t.Errorf("%+v: JevOff %v reasons %q", c.in, d.JevOff, d.Reasons)
		}
		if d.Judgments != nil {
			t.Errorf("%+v: Judgments %v with Jev off, want none", c.in, d.Judgments)
		}
	}
}

func TestWorkerJudgmentsWithoutJev(t *testing.T) {
	j := WorkerJudgmentsWithoutJev(WorkerInput{Size: "L", Type: "release-paperwork", Hint: tier("fable", "low")})
	if j.Mechanical != 1 || j.DesignOpen != 0 || j.Security != 0 || j.LiveData != 0 || j.RootCause != 0 || j.Visual != 0 {
		t.Errorf("release-paperwork, fable/low hint: %+v", j)
	}
	j = WorkerJudgmentsWithoutJev(WorkerInput{Size: "S", Type: "ui", Hint: tier("opus", "xhigh")})
	if j.Visual != 1 || j.DesignOpen != 0 || j.Role != "" {
		t.Errorf("ui, size S: %+v", j)
	}
	j = WorkerJudgmentsWithoutJev(WorkerInput{Size: "L", Type: "research", Hint: tier("fable", "medium")})
	if j.Role != "research" || j.DesignOpen != 1 {
		t.Errorf("research, size L, fable/medium hint: %+v", j)
	}
}

// requestBytes is the JSON size of a Jev request as the client encodes it.
func requestBytes(t *testing.T, v any) int {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Len()
}

func TestWorkerSplitsLongBrief(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 160000; i++ {
		b.WriteString("## Section \"quoted\"\n\tStep <a> & <b>: rename the helper and update its callers.\n")
	}
	brief := b.String()[:160000]
	ask := &fakeAsker{answer: func(n int) (map[string]typesafe.Answer, error) {
		switch n {
		case 0:
			return answers("implement", 0.6, map[string]float64{"mechanical": 0.9, "security": 0.1, "visual": 0.2}), nil
		case 1:
			return answers("research", 0.95, map[string]float64{"mechanical": 0.2, "security": 0.25, "root_cause": 0.4}), nil
		default:
			return answers("review", 0.5, map[string]float64{"mechanical": 1}), nil
		}
	}}
	d, err := DecideWorker(context.Background(), WorkerInput{Brief: brief, Size: "M", Type: "feature"}, ask, &fakeGate{}, noLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(ask.states) < 2 {
		t.Fatalf("%d Jev requests for a 160,000-character brief, want the brief split", len(ask.states))
	}
	var joined strings.Builder
	for i, s := range ask.states {
		state := s.(map[string]any)
		joined.WriteString(state[policy.WorkerStateField].(string))
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
	if joined.String() != brief {
		t.Error("the sections do not add up to the brief")
	}
	j := d.Judgments
	if j["role"] != "research" || j["mechanical"] != 0.2 || j["security"] != 0.25 || j["root_cause"] != 0.4 || j["visual"] != 0.2 || j["design_open"] != 0.0 {
		t.Errorf("combined judgments %v", j)
	}
	if d.Row != "research" {
		t.Errorf("row %q, want research", d.Row)
	}
}

func TestSplitStateLongLine(t *testing.T) {
	qs := map[string]typesafe.Question{"q": {Type: "noul", Instructions: "Is it?"}}
	text := strings.Repeat("é\"", 100000) // one line, escaping doubles every quote
	states, err := SplitState("f", text, qs)
	if err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for _, s := range states {
		if tok := (requestBytes(t, s) + requestBytes(t, qs["q"])) / policy.CharsPerToken; tok >= policy.JevQuestionTokens {
			t.Errorf("state of %d tokens", tok)
		}
		joined.WriteString(s["f"].(string))
	}
	if joined.String() != text {
		t.Error("the sections do not add up to the text")
	}
	if got, err := SplitState("f", "short", qs); err != nil || len(got) != 1 || got[0]["f"] != "short" {
		t.Errorf("short text: %v, %v", got, err)
	}
}

func TestCodeRisk(t *testing.T) {
	for _, c := range []struct {
		text  string
		paths []string
		want  bool
	}{
		{"Add OAuth login.", nil, true},
		{"Update the nginx config and DEPLOY it.", nil, true},
		{"Rename a helper.", []string{"internal/auth/session.go"}, true},
		{"Rename a helper.", []string{"internal/tokenizer/split.go"}, false},
		{"Rework the authentication middleware.", nil, true},
		{"Rotate the API tokens and secrets.", nil, true},
		{"Plan the deployment.", nil, true},
		{"Squash the migrations.", []string{"db/migrations/001.sql"}, true},
		{"Refund failed payments.", nil, true},
		{"Fix the authority list layout.", nil, false},
		{"Never commit secrets; do not deploy.", nil, false},
		{"Fix the header spacing.", []string{"web/header.css"}, false},
	} {
		if got := CodeRisk(c.text, c.paths); got != c.want {
			t.Errorf("CodeRisk(%q, %q) = %v, want %v", c.text, c.paths, got, c.want)
		}
	}
}

func TestNoJudgmentsWithoutJevAnswers(t *testing.T) {
	failing := &fakeAsker{answer: func(int) (map[string]typesafe.Answer, error) { return nil, errors.New("typesafe: HTTP 503") }}
	cases := []struct {
		name   string
		ask    *fakeAsker
		gate   *fakeGate
		row    string
		reason string
	}{
		{"local", jev("implement", nil), &fakeGate{res: &privacy.Result{Local: true, Reason: privacy.ReasonStillPrivate}}, "local", "privacy gate kept the brief local: " + privacy.ReasonStillPrivate},
		{"gate error", jev("implement", nil), &fakeGate{err: errors.New("helper crashed")}, "local-error", "privacy gate failed: helper crashed"},
		{"Jev error", failing, &fakeGate{}, "local-error", "Jev failed: typesafe: HTTP 503"},
	}
	for _, c := range cases {
		d, err := DecideWorker(context.Background(), WorkerInput{Brief: "b", Size: "M"}, c.ask, c.gate, noLimits)
		if err != nil {
			t.Fatal(err)
		}
		if d.Row != c.row || d.Judgments != nil || !hasReason(d, c.reason) {
			t.Errorf("%s: row %q judgments %v reasons %q; want row %q, no judgments, reason %q", c.name, d.Row, d.Judgments, d.Reasons, c.row, c.reason)
		}
	}
}

func TestSplitStateRefusesNoRoom(t *testing.T) {
	big := map[string]typesafe.Question{"q": {Type: "noul", Instructions: strings.Repeat("x", policy.JevQuestionTokens*policy.CharsPerToken)}}
	if states, err := SplitState("f", "short brief", big); err == nil {
		t.Errorf("no error; %d states", len(states))
	}
	bad := map[string]typesafe.Question{"q": {Type: "noul", Criteria: func() {}}}
	if _, err := SplitState("f", "short brief", bad); err == nil {
		t.Error("an unencodable question: no error")
	}
}

// TestSplitCountsLikeTheClient pins SplitState's size model to the bytes the
// TypeSafe client really sends, so the two cannot drift apart silently.
func TestSplitCountsLikeTheClient(t *testing.T) {
	var sent []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, len(body))
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := typesafe.New("test-key")
	c.BaseURL = srv.URL
	qs := map[string]typesafe.Question{}
	for id, q := range policy.WorkerQuestions {
		qs[id] = typesafe.Question(q)
	}
	states, err := SplitState(policy.WorkerStateField, "Step <1> & \"two\"\n\tdone", qs)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Ask(context.Background(), states[0], qs)
	want, err := jsonLen(map[string]any{"model": policy.Model, "state": states[0], "questions": qs})
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != want+1 { // the client's encoder ends with a newline
		t.Errorf("client sent %v bytes, SplitState counts %d", sent, want+1)
	}
}

// TestTypeResearchWithJev: --type research makes a research worker even when
// Jev reads the brief as implementation.
func TestTypeResearchWithJev(t *testing.T) {
	d, err := DecideWorker(context.Background(), WorkerInput{Brief: "Compare the two queue libraries.", Size: "M", Type: "research"},
		jev("implement", nil), &fakeGate{}, limits.ClaudeState{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Row != "research" || d.Tier != (Tier{"sonnet", "medium"}) {
		t.Errorf("row %s tier %+v, want research sonnet/medium", d.Row, d.Tier)
	}
}
