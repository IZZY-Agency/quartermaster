// Package providers detects the optional providers (design 5.10): a missing
// one is a state that turns its feature off, shown by qm status.
package providers

// Status says whether one optional provider is on, and why not when it is off.
type Status struct {
	Name string `json:"name"`
	On   bool   `json:"on"`
	Why  string `json:"why,omitempty"`
	// Fix is a command that turns the provider on, when there is one.
	Fix string `json:"fix,omitempty"`
}
