// Package typesafe talks to TypeSafe's Jev model. This file holds the shared
// question and answer types; the client comes later.
package typesafe

// Question is one typed question. Type: noul|choice|score.
type Question struct {
	Type         string
	Instructions string
	Criteria     any
}

// Answer is Jev's answer to one Question; the field that matches Type is set.
type Answer struct {
	Type          string
	Noul          float64
	Choice        string
	Score         float64
	Probabilities map[string]float64
}
