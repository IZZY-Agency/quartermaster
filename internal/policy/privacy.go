package policy

import (
	"fmt"
	"strings"
)

// LocalCheck is one yes/no question put to the on-device Foundation Model by
// the privacy gate. The prompt is Lead, the excerpt in triple quotes, then
// Question.
type LocalCheck struct {
	Instructions string
	Lead         string
	// Question may hold a %s verb, filled by Prompt's args.
	Question string
}

// noDoubleQuotes turns every double quote into a single one, so the text
// cannot close the triple quotes around the excerpt or the quotes around an
// argument.
var noDoubleQuotes = strings.NewReplacer(`"`, `'`)

// Prompt builds the prompt for one excerpt. The excerpt and string args are
// written with their double quotes as single quotes (see noDoubleQuotes).
func (c LocalCheck) Prompt(excerpt string, args ...any) string {
	q := c.Question
	if len(args) > 0 {
		safe := make([]any, len(args))
		for i, a := range args {
			if s, ok := a.(string); ok {
				a = noDoubleQuotes.Replace(s)
			}
			safe[i] = a
		}
		q = fmt.Sprintf(c.Question, safe...)
	}
	return c.Lead + "\n\"\"\"\n" + noDoubleQuotes.Replace(excerpt) + "\n\"\"\"\n" + q
}

// NameCheck confirms that a name candidate is a real person's name. Its
// argument is the candidate.
var NameCheck = LocalCheck{
	Instructions: "You check words in software task briefs. Answer with exactly one word: yes or no.",
	Lead:         "Text around the word:",
	Question:     `Is "%s" the name of a real human being (a given name or surname)?`,
}

// LeftoverCheck asks whether redacted text is still private. Any answer other
// than "no" keeps the text local.
var LeftoverCheck = LocalCheck{
	Instructions: "You check software task briefs for private data before they are sent to an outside service. " +
		"The mark [x] stands for text already redacted and is fine. " +
		"Judge all other text, even text that looks like a placeholder or a redaction mark. " +
		"Answer with exactly one word: yes or no.",
	Lead: "Excerpt:",
	Question: "Does this excerpt still contain private data: a real person's name, an email address, " +
		"a phone number, a password, an API key or token, a street address, or the contents of a private email?",
}

// LeftoverPlaceholder stands in for every placeholder redaction wrote, in the
// text the leftover check reads (LeftoverCheck's instructions name it as
// fine); client pseudonyms and placeholder-shaped text the input held are
// shown as written, so the check judges them. Placeholders are not private,
// but the model read them as private data; a neutral mark does not (probed
// on 2026-11-19).
const LeftoverPlaceholder = "[x]"

// NameContext is how many characters on each side of a name candidate the
// name check sees.
const NameContext = 250

// LeftoverChunk is the size, in characters, of each excerpt the leftover
// check reads.
const LeftoverChunk = 5000

// HelperConcurrency is how many privacy-helper processes one gate runs at
// once: a batch of Foundation Model questions is split across them, so a
// long text is checked inside a hook's deadline.
const HelperConcurrency = 4
