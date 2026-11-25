package up

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
	"github.com/IZZY-Agency/quartermaster/internal/route"
)

const (
	modelArg    = "--model"
	fallbackArg = "--fallback-model"
)

// actionScript is the action of an "up" Record of a session whose launch line
// was written and that is not known to have started.
const actionScript = "script"

// CoordinatorModel is the config's model and fallback for a coordinator that
// starts from scratch (up.coordinator_model, up.coordinator_fallback), ""
// for none. The notes say what was wrong with the config's value of each.
type CoordinatorModel struct {
	Model, Fallback         string
	ModelNote, FallbackNote string
}

// ModelChoice is what qm up adds to a coordinator's own claude arguments:
// Model and Fallback ("" for neither). Text names the model and fallback the
// coordinator starts with, and where each comes from ("" for a resumed one
// that gets nothing); Note is a warning about the config's values, or about
// a claude they cannot be used with.
type ModelChoice struct {
	Model, Fallback string
	Text, Note      string
}

// FreshModel is the model and fallback of a coordinator that starts from
// scratch with its own claude arguments args: those of the config (d.Model),
// when args name neither and claude can use them (d.ModelCheck). An explicit
// --model leaves both out, an explicit --fallback-model the config's
// fallback, and the model too when it names the same one: claude may refuse
// a fallback that is its main model.
func FreshModel(args []string, d Deps) ModelChoice {
	model, hasModel := argValue(args, modelArg)
	fallback, hasFallback := argValue(args, fallbackArg)
	var c ModelChoice
	if !hasModel {
		c.Model = d.Model.Model
		notes := nonEmpty(d.Model.ModelNote)
		switch {
		case hasFallback:
			if namesModel(fallback, c.Model) {
				c.Model = ""
			}
		case c.Model != "":
			notes = append(notes, nonEmpty(d.Model.FallbackNote)...)
			if d.Model.Fallback != c.Model {
				c.Fallback = d.Model.Fallback
			}
		}
		c.Note = strings.Join(notes, "; ")
		c.check(d)
	}
	c.Text = modelText("model", model, hasModel, c.Model, "up.coordinator_model") + ", " +
		modelText("fallback", fallback, hasFallback, c.Fallback, "up.coordinator_fallback")
	return c
}

// resumeModel is what the resume of coordinator session sid adds: the
// config's fallback, when qm started that session with its default model
// (Project.DefaultModels) and the project's arguments name neither a model
// nor a fallback. claude keeps the session's model on a resume, but not its
// fallback, so an account without access to the model could not go on with
// the session; that holds for the sessions already started when the config's
// model is emptied, too. Any other session gets nothing.
func resumeModel(p *Project, sid string, d Deps) ModelChoice {
	started := p.DefaultModels[sid]
	if started == "" || explicitModel(p.ClaudeArgs) {
		return ModelChoice{}
	}
	c := ModelChoice{Note: d.Model.FallbackNote}
	if d.Model.Fallback != started {
		c.Fallback = d.Model.Fallback
	}
	if c.check(d); c.Fallback != "" {
		c.Text = modelText("fallback", "", false, c.Fallback, "up.coordinator_fallback")
	}
	return c
}

// StepModel is the model and fallback the coordinator step s of project p
// starts with, its own claude arguments being args; none for any other step
// or action.
func StepModel(p *Project, s Step, args []string, d Deps) ModelChoice {
	switch {
	case s.Role != RoleCoordinator:
	case s.Action == Fresh:
		return FreshModel(args, d)
	case s.Action == Resume:
		q := *p
		q.ClaudeArgs = args
		return resumeModel(&q, s.SessionID, d)
	}
	return ModelChoice{}
}

// check drops the model and the fallback, with a note, when claude cannot use
// them: one that does not know --fallback-model would not start, and the
// model without its fallback would fail an account without access to it.
func (c *ModelChoice) check(d Deps) {
	if d.ModelCheck == nil || (c.Model == "" && c.Fallback == "") {
		return
	}
	if why := d.ModelCheck(); why != "" {
		c.Model, c.Fallback = "", ""
		c.Note = strings.Join(append(nonEmpty(c.Note), why), "; ")
	}
}

// flags are the claude arguments the choice adds.
func (c ModelChoice) flags() []string {
	var out []string
	if c.Model != "" {
		out = append(out, modelArg, c.Model)
	}
	if c.Fallback != "" {
		out = append(out, fallbackArg, c.Fallback)
	}
	return out
}

// modelText is one part of a choice's Text: the value the user gave, else the
// one qm adds from config value key.
func modelText(what, given string, isGiven bool, added, key string) string {
	switch {
	case isGiven:
		return fmt.Sprintf("%s %s (its claude arguments)", what, oneLine(given))
	case added != "":
		return fmt.Sprintf("%s %s (%s)", what, added, key)
	}
	return what + " not set"
}

// argValue is the value of the last flag in args (--flag V or --flag=V), and
// whether the flag is there at all.
func argValue(args []string, flag string) (value string, ok bool) {
	for i, a := range args {
		if v, eq := strings.CutPrefix(a, flag+"="); eq {
			value, ok = v, true
		} else if a == flag {
			value, ok = "", true
			if i+1 < len(args) {
				value = args[i+1]
			}
		}
	}
	return value, ok
}

// explicitModel says whether args name a model or a fallback.
func explicitModel(args []string) bool {
	_, model := argValue(args, modelArg)
	_, fallback := argValue(args, fallbackArg)
	return model || fallback
}

// namesModel says whether list, a --fallback-model value (comma separated),
// names model, by routing's name or its Claude model id.
func namesModel(list, model string) bool {
	return model != "" && slices.ContainsFunc(strings.Split(list, ","), func(m string) bool {
		m = strings.TrimSpace(m)
		return m == model || m == policy.ModelIDs[model]
	})
}

// ReplaceClaudeArgs makes args the coordinator's own claude arguments. Ones
// that name a model or a fallback end qm's defaults for the sessions started
// so far: they are the user's choice now.
func (p *Project) ReplaceClaudeArgs(args []string) {
	p.ClaudeArgs = append([]string{}, args...)
	if explicitModel(args) {
		for id := range p.DefaultModels {
			p.DefaultModels[id] = ""
		}
	}
}

// setDefaultModel remembers that qm starts session sid with its default model
// ("" : it does not).
func (p *Project) setDefaultModel(sid, model string) {
	if model == "" {
		return
	}
	if p.DefaultModels == nil {
		p.DefaultModels = map[string]string{}
	}
	p.DefaultModels[sid] = model
}

// withoutDefaultModel is kept, the flags adoption keeps of running session
// ids (its id, and the one its arguments name, which differ after /clear),
// without the model qm itself gave it by default, so it is not saved as the
// user's choice; left says it was left out. That model is in the last Record
// of a start: the other Records of a session say nothing about it.
func withoutDefaultModel(kept, ids []string, recs []route.Record) (_ []string, left bool) {
	model := ""
	for _, r := range recs {
		if r.Kind != "up" || r.ClaudeSession == "" || !slices.Contains(ids, r.ClaudeSession) {
			continue
		}
		switch r.Action {
		case string(Fresh), string(Resume), actionScript:
			model = r.DefaultModel
		}
	}
	if model == "" {
		return kept, false
	}
	if i := slices.Index(kept, modelArg); i >= 0 && i+1 < len(kept) && kept[i+1] == model {
		return slices.Delete(slices.Clone(kept), i, i+2), true
	}
	return kept, false
}

// logScript logs the "up" Record of coordinator step s, whose launch line
// starts session id, when qm does not know that it started (no launcher, or a
// launch that failed or may have) and has given that session a default model
// at some time: an adoption without the project's state reads it there
// (withoutDefaultModel).
func logScript(d Deps, p *Project, s Step, id string, now time.Time, o *Outcome) {
	if _, ok := p.DefaultModels[id]; !ok || s.Role != RoleCoordinator || o.Script == "" {
		return
	}
	s.Action = actionScript
	logUp(d, p, s, id, now, o)
}
