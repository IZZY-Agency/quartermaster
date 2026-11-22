package route

import "github.com/IZZY-Agency/quartermaster/internal/policy"

// CodeRisk reports whether text or one of paths names authentication,
// payments, deployment, migrations, secrets or exposure (policy.RiskIn). It
// counts as risk whatever Jev says.
func CodeRisk(text string, paths []string) bool {
	if policy.RiskIn(text) {
		return true
	}
	for _, p := range paths {
		if policy.RiskIn(p) {
			return true
		}
	}
	return false
}

// workerCodeRisk is the code risk signal of a worker: the brief, or type
// migration.
func workerCodeRisk(in WorkerInput) bool {
	return in.Type == "migration" || CodeRisk(in.Brief, nil)
}
