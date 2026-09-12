package draw

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// Mermaid writes the resolved workflow as a Mermaid flowchart, left to right.
//
// Mermaid has no ports, so what DOT anchors the label says: every edge carries the port it
// leaves by and the port it feeds, which is the fact an edge of this language is made of.
// The boundary is a dashed link to a rounded node, one per declared workflow input and
// output, for the same reason it is dashed in DOT: an output is a view of a step port and
// not a step.
//
// This exists beside DOT rather than instead of it because a merge request renders Mermaid
// and a terminal renders DOT, and neither is a conversion of the other: the same reading
// written twice is two files that cannot drift, where a converter would be a third thing to
// keep in step.
func Mermaid(w io.Writer, wf *graph.Workflow) error {
	d, err := read(wf)
	if err != nil {
		return err
	}
	out := bufio.NewWriter(w)
	ids := newIdentifiers(d)

	fmt.Fprint(out, "flowchart LR\n")
	for _, n := range d.Nodes {
		fmt.Fprintf(out, "\t%s[%s]\n", ids.step(n.Name), label(append([]string{string(n.Name)}, n.Rows...)))
	}

	if len(d.Links) > 0 {
		fmt.Fprint(out, "\n")
	}
	for _, l := range d.Links {
		fmt.Fprintf(out, "\t%s -->|%s| %s\n",
			ids.step(l.From), label([]string{string(l.FromPort) + " to " + string(l.ToPort)}), ids.step(l.To))
	}

	if len(d.Inputs) > 0 || len(d.Outputs) > 0 {
		fmt.Fprint(out, "\n")
	}
	for _, b := range d.Inputs {
		id := ids.boundary("input", b.Name)
		fmt.Fprintf(out, "\t%s([%s])\n", id, label([]string{b.Name}))
		fmt.Fprintf(out, "\t%s -. %s .-> %s\n", id, label([]string{string(b.Port)}), ids.step(b.Step))
	}
	for _, b := range d.Outputs {
		id := ids.boundary("output", b.Name)
		fmt.Fprintf(out, "\t%s([%s])\n", id, label([]string{b.Name}))
		fmt.Fprintf(out, "\t%s -. %s .-> %s\n", ids.step(b.Step), label([]string{string(b.Port)}), id)
	}
	return out.Flush()
}

// label writes one node or edge label: the lines of it, quoted, with every character the
// renderer would read as markup written as the entity Mermaid reads back.
//
// The lines are joined with a line break rather than with a separator of this package's
// invention, because a node carrying three facts on one line is a node nobody can read.
func label(lines []string) string {
	escaped := make([]string, 0, len(lines))
	for _, line := range lines {
		escaped = append(escaped, entities(line))
	}
	return `"` + strings.Join(escaped, "<br/>") + `"`
}

// entities writes what a Mermaid label cannot carry as itself.
//
// A label is rendered as markup, so an expression written ${{ inputs.in.count > 0 }} would
// lose its comparison and a quotation mark would end the label. Each becomes the numeric
// entity, which is the one escape the language states and the one every renderer of it
// reads.
func entities(s string) string {
	return strings.NewReplacer(
		"#", "#35;",
		"&", "#38;",
		"\"", "#34;",
		"<", "#60;",
		">", "#62;",
	).Replace(s)
}

// identifiers are the node names of one drawing.
//
// A Mermaid node identifier is not a string the way a DOT one is, and a step name carries a
// hyphen the flowchart grammar reads as part of a link, so the identifier is derived and the
// label carries the name itself. It is derived once per drawing and held, so that a step
// named fan-out and a step named fan_out, which sanitise to the same word, still get one
// identifier each: two steps drawn as one node would draw a graph the file does not
// describe.
type identifiers struct {
	steps      map[agk.Step]string
	boundaries map[string]string
	taken      map[string]bool
}

func newIdentifiers(d *drawing) *identifiers {
	ids := &identifiers{steps: map[agk.Step]string{}, boundaries: map[string]string{}, taken: map[string]bool{}}
	for _, n := range d.Nodes {
		ids.steps[n.Name] = ids.claim("step_" + sanitise(string(n.Name)))
	}
	return ids
}

// step is the identifier of one step, which is a word this drawing already claimed.
//
// An edge naming a step the file does not declare cannot reach here, because Check refuses
// one before anything is drawn; a name with no identifier is answered with the derived one
// all the same rather than with nothing, since a drawing that silently dropped an edge would
// be worse than one naming a box nobody declared.
func (i *identifiers) step(name agk.Step) string {
	if id, ok := i.steps[name]; ok {
		return id
	}
	id := i.claim("step_" + sanitise(string(name)))
	i.steps[name] = id
	return id
}

// boundary is the identifier of one end of the workflow's boundary.
//
// It is remembered by the name the file gives it and not by the word that name sanitises
// to, for the reason a step's identifier is claimed: an input named start-date and an input
// named start_date are two declared inputs of one workflow, and answering the second with
// the first's node would draw one input where the file declares two and hang both edges off
// it. The same input asked for twice is the same node, which is what the lookup is for: an
// input is drawn once per port it feeds.
func (i *identifiers) boundary(kind, name string) string {
	key := kind + "\x00" + name
	if id, taken := i.boundaries[key]; taken {
		return id
	}
	id := i.claim(kind + "_" + sanitise(name))
	i.boundaries[key] = id
	return id
}

// claim takes one identifier, or the first numbered one after it that is free.
func (i *identifiers) claim(want string) string {
	id := want
	for n := 2; i.taken[id]; n++ {
		id = fmt.Sprintf("%s_%d", want, n)
	}
	i.taken[id] = true
	return id
}

// sanitise keeps what a flowchart identifier may carry, which is letters, digits and the
// underscore.
func sanitise(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
