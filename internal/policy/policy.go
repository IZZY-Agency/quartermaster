// Package policy holds every Jev question, threshold and table. Nothing else
// in quartermaster words a question or sets a band.
package policy

// Model is the Jev model every question is asked of.
const Model = "jev-1.14.0"

// Noul bands: at or above YesAt is yes, at or below NoAt is no, between is
// uncertain (for risk questions, uncertain counts as risk).
const (
	YesAt = 0.7
	NoAt  = 0.3
)
