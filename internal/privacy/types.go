// Package privacy redacts private material before text is sent to Jev. This
// file holds the shared result type; the gate comes later.
package privacy

// Result is the gated text and what the gate did to it.
type Result struct {
	Text   string         `json:"text"`
	Local  bool           `json:"local"`
	Reason string         `json:"reason,omitempty"`
	Counts map[string]int `json:"counts,omitempty"`
	// Off lists the checks skipped because their provider is missing.
	Off []string `json:"off,omitempty"`
}
