package policy

// ModelIDs maps the model names routing uses (sonnet, opus, fable, haiku) to
// the Claude model ids passed to claude --model.
var ModelIDs = map[string]string{
	"sonnet": "claude-sonnet-5-5",
	"opus":   "claude-opus-5-5",
	"fable":  "claude-fable-5-2",
	"haiku":  "claude-haiku-5-5",
}
