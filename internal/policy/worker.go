package policy

import "regexp"

// Question is one typed Jev question, worded here and converted to
// typesafe.Question by the router (typesafe imports this package, so this
// package cannot import it). Type: noul|choice|score. It must keep the same
// fields, in the same order, as typesafe.Question so the conversion compiles.
type Question struct {
	Type         string
	Instructions string
	Criteria     any
}

// Tier is a Claude model and effort, as a row of a routing table gives it. It
// must keep the same fields, in the same order, as route.Tier so the
// conversion compiles.
type Tier struct {
	Model  string
	Effort string
}

// RiskPattern matches the phrases that make a brief or a changed path risky
// on their own: authentication and access control, credentials and secrets,
// payments, deployment, migrations and exposure. Words that only look alike
// (author, token budget, tokenizer, git checkout, migrate the tests) do not
// match. A match whose "negated" group is set is standing boilerplate ("do not
// deploy", "never commit secrets") and does not count; see RiskIn. Jev covers
// the nuance when it is on.
const RiskPattern = `(?i)(?P<negated>\b(?:do not|don't|never|must not|should not|no need to)\s+(?:[\w.-]+\s+){0,2})?` +
	`\b(?:` +
	// authentication and access control
	`o?auth(?:enticat\w*|orization|orized?|n|z)?\b|log-?ins?\b|sign-?in\b|sso\b|saml\b|jwt\b|csrf\b|cors\b|` +
	`2fa\b|mfa\b|two-factor|access[_ -]control|rbac\b|session[_ -]cookies?\b|` +
	// credentials and secrets
	`passwords?\b|passphrases?\b|credentials?\b|secrets?\b|api[_ -]?keys?\b|private[_ -]keys?\b|ssh[_ -]keys?\b|bearer\b|` +
	`(?:api|access|auth|bearer|refresh|session|csrf|id|secret)[_ -]?tokens?\b|` +
	// payments
	`payments?\b|billing\b|invoic\w*|refunds?\b|stripe\b|mollie\b|paypal\b|checkout[ -](?:flow|page|process|form|session|step)s?\b|` +
	// deployment, migrations, exposure
	`deploy\w*|migrations?\b|migrate\s+(?:the\s+)?(?:database|db|schema|data)\b|firewalls?\b|nginx\b|open\s+(?:a\s+|the\s+)?ports?\b` +
	`)`

// riskRe is RiskPattern compiled; negatedGroup is its "negated" group.
var (
	riskRe       = regexp.MustCompile(RiskPattern)
	negatedGroup = riskRe.SubexpIndex("negated")
)

// RiskIn reports whether text holds a RiskPattern match that is not negated
// boilerplate.
func RiskIn(text string) bool {
	for _, m := range riskRe.FindAllStringSubmatchIndex(text, -1) {
		if m[2*negatedGroup] < 0 {
			return true
		}
	}
	return false
}

// WorkerStateField is the state field that holds the redacted worker brief.
const WorkerStateField = "task_brief"

// WorkerQuestions are the questions Jev answers about a worker brief, worded
// as sent on 2026-11-19.
var WorkerQuestions = map[string]Question{
	"role": {
		Type:         "choice",
		Instructions: "What kind of job does `task_brief` give the worker?",
		Criteria: map[string]string{
			"implement": "The worker writes or changes code, config or content and delivers it, for example as a PR, a deploy or a fix.",
			"review":    "The worker reads work someone else made and reports findings, without changing that work.",
			"research":  "The worker investigates, designs or critiques and reports back, without changing product code.",
		},
	},
	"mechanical": {
		Type:         "noul",
		Instructions: "Is the work in `task_brief` mostly mechanical, such as applying a listed set of fixes, following exact steps, or copying a known pattern?",
	},
	"security": {
		Type:         "noul",
		Instructions: "Does the task in `task_brief` change authentication, access control, secrets, payments, or what a server exposes to the internet?",
	},
	"live_data": {
		Type:         "noul",
		Instructions: "Does the task in `task_brief` act on a live production system or on real customer data, for example by deploying, migrating, or changing a running server?",
		Criteria: map[string]string{
			"true":  "The worker itself deploys, migrates or changes something live.",
			"false": "The worker only changes code in a branch or a local copy, for others to release later.",
		},
	},
	"root_cause": {
		Type:         "noul",
		Instructions: "Must the worker in `task_brief` find the cause of a bug or failure whose cause is not yet known?",
	},
	"design_open": {
		Type:         "noul",
		Instructions: "Does `task_brief` leave important design or architecture decisions for the worker to make on its own?",
		Criteria: map[string]string{
			"true":  "The brief gives a goal and the worker must choose the approach, data shape or architecture.",
			"false": "The brief states the approach and the decisions, and the worker follows them.",
		},
	},
	"visual": {
		Type:         "noul",
		Instructions: "Is the visual design quality of a user interface a main part of what the worker in `task_brief` delivers?",
	},
}

// WorkerTiers is the tier of each worker-table row; the conditions are in
// route.WorkerTable. The force and local rows take their tier from the
// coordinator, and research-open is the research row when design is open.
var WorkerTiers = map[string]Tier{
	"local":         {"opus", "high"}, // the local row without a hint
	"research":      {"sonnet", "medium"},
	"research-open": {"opus", "medium"},
	"risk-big":      {"opus", "xhigh"},
	"risk":          {"opus", "high"},
	"ui":            {"opus", "medium"},
	"s-light":       {"sonnet", "low"},
	"s":             {"sonnet", "medium"},
	"m-open":        {"opus", "medium"},
	"l":             {"opus", "medium"},
	"m":             {"sonnet", "high"},
}

// LocalCap is the highest tier the local rows take from the coordinator's
// hint: a stronger model or effort is lowered to it.
var LocalCap = Tier{"opus", "high"}

// DownshiftAbove is the Claude 5-hour usage, in percent, above which a
// non-risk row moves one step down DownshiftLadder.
const DownshiftAbove = 90.0

// DownshiftLadder lists the tiers a downshift steps through, strongest first.
var DownshiftLadder = []Tier{
	{"opus", "medium"},
	{"sonnet", "high"},
	{"sonnet", "medium"},
	{"sonnet", "low"},
}

// NoDownshiftRows are the rows the downshift never touches: the coordinator's
// own pick, the local rows (risk unknown) and the ui floor (at least
// opus/medium).
var NoDownshiftRows = []string{"force", "local", "local-error", "ui"}

// Jev request limits, in tokens, and the rate used to count them.
const (
	JevRequestTokens  = 64000
	JevQuestionTokens = 32000
	CharsPerToken     = 4
)
