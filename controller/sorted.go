package controller

import (
	"sort"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// Two orders, so that a document written twice from one state is the same bytes twice.
//
// Go randomises map iteration, and a document whose envelope list came out in a different order
// each pass would be a jsonb column rewritten with the same content and a diff nobody could
// read.

func sortedSteps(s *graph.State) []agk.Step {
	out := make([]agk.Step, 0, len(s.Steps))
	for name := range s.Steps {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedPorts(m map[agk.Port]agk.Envelope) []agk.Port {
	out := make([]agk.Port, 0, len(m))
	for port := range m {
		out = append(out, port)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
