package policy

// State fields of the ask gate's Jev request.
const (
	AskConversationField = "recent_conversation"
	AskQuestionField     = "question"
)

// AskQuestions are the questions Jev answers before the coordinator asks the
// user something (design 5.7), worded as sent on 2026-11-19. The ask gate
// denies the question when answerable_from_context and cheap_to_undo are yes
// (at or above YesAt) and product_ruling is no (at or below NoAt).
var AskQuestions = map[string]Question{
	"answerable_from_context": {
		Type:         "noul",
		Instructions: "Can the answer to `question` be found in `recent_conversation` or in the code it describes?",
	},
	"cheap_to_undo": {
		Type:         "noul",
		Instructions: "If the assistant guesses the answer to `question` wrongly, can the mistake be undone cheaply?",
	},
	"product_ruling": {
		Type:         "noul",
		Instructions: "Is `question` a product or business decision that only the owner can make?",
	},
}

// AskTurns is how many of the latest user and assistant text turns of the
// transcript the ask gate sends as recent_conversation.
const AskTurns = 20

// AskContextChars caps the recent conversation, in bytes: the most recent
// turns are kept, the oldest dropped first. It keeps the request under
// JevQuestionTokens and bounds the privacy gate's time (one Foundation Model
// call per leftover chunk and per name candidate) inside the hook's deadline.
const AskContextChars = 12000

// AskQuestionChars caps the question the ask gate sends, in bytes.
const AskQuestionChars = 4000

// State fields of the feedback analysis' Jev requests.
const (
	FeedbackMessageField = "message"
	FeedbackRuleField    = "rule"
)

// FeedbackLastingRule asks whether the user's message states a lasting rule
// (design 5.7), worded as sent on 2026-11-19.
var FeedbackLastingRule = map[string]Question{
	"lasting_rule": {
		Type:         "noul",
		Instructions: "Does `message` state a lasting rule about how agents should work in future?",
	},
}

// FeedbackCovered asks whether one rule already written down covers the
// message; it is asked once per candidate rule.
var FeedbackCovered = map[string]Question{
	"covered": {
		Type:         "noul",
		Instructions: "Does `rule` already say what `message` asks for?",
	},
}

// FeedbackCandidates is how many written rules (CLAUDE.md bullets and memory
// index lines, those sharing the most words with the message) are checked
// against a message that states a lasting rule.
const FeedbackCandidates = 12

// FeedbackMaxChars: a message longer than this, in bytes, is not
// analyzed. A lasting rule is a short statement; a long message is a paste.
const FeedbackMaxChars = 20000
