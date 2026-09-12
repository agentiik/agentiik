package draw

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/agentiik/agentiik/agk"
	"github.com/agentiik/agentiik/graph"
)

// DOT writes the resolved workflow as a DOT digraph.
//
// One node per step, drawn as a table so that a port is a place on the box and not a word
// in a label: an input port is anchored on the left of the step it feeds and an output
// port on the right of the step that writes it, and every edge is anchored from the port it
// leaves by to the port it feeds. An edge in the picture is therefore an edge in the file
// rather than a line between two boxes.
//
// The workflow's own boundary is dashed: one node per declared input and one per declared
// output. An output is a view of a step port and not a step, and drawing it like a step
// would put a box in the graph that nothing runs.
//
// No colour, no theme, no layout and no font. dot is better at all four than this would be,
// and a drawing carrying a palette is a drawing that disagrees with whatever renders it.
func DOT(w io.Writer, wf *graph.Workflow) error {
	d, err := read(wf)
	if err != nil {
		return err
	}
	out := bufio.NewWriter(w)

	fmt.Fprintf(out, "digraph %s {\n", quote(d.Title))
	// Left to right, because a workflow is read in the direction its edges point, and
	// plain nodes, because the label is the whole of the box.
	fmt.Fprint(out, "\trankdir=LR\n")
	fmt.Fprint(out, "\tnode [shape=plain]\n")

	for _, n := range d.Nodes {
		fmt.Fprint(out, "\n")
		fmt.Fprintf(out, "\t%s [label=<%s>]\n", quote(string(n.Name)), table(n))
	}

	if len(d.Links) > 0 {
		fmt.Fprint(out, "\n")
	}
	for _, l := range d.Links {
		fmt.Fprintf(out, "\t%s:%s:e -> %s:%s:w\n",
			quote(string(l.From)), outAnchor(l.FromPort),
			quote(string(l.To)), inAnchor(l.ToPort))
	}

	if len(d.Inputs) > 0 || len(d.Outputs) > 0 {
		fmt.Fprint(out, "\n")
	}
	for _, b := range d.Inputs {
		id := "input:" + b.Name
		fmt.Fprintf(out, "\t%s [shape=box style=dashed label=%s]\n", quote(id), quote(b.Name))
		fmt.Fprintf(out, "\t%s -> %s:%s:w [style=dashed]\n", quote(id), quote(string(b.Step)), inAnchor(b.Port))
	}
	for _, b := range d.Outputs {
		id := "output:" + b.Name
		fmt.Fprintf(out, "\t%s [shape=box style=dashed label=%s]\n", quote(id), quote(b.Name))
		fmt.Fprintf(out, "\t%s:%s:e -> %s [style=dashed]\n", quote(string(b.Step)), outAnchor(b.Port), quote(id))
	}

	fmt.Fprint(out, "}\n")
	return out.Flush()
}

// table is one step as an HTML-like label: its name, the keywords that belong inside the
// box, then one row per rank of ports with the inputs on the left and the outputs on the
// right.
func table(n node) string {
	var b strings.Builder
	b.WriteString(`<TABLE BORDER="0" CELLBORDER="1" CELLSPACING="0" CELLPADDING="4">`)
	fmt.Fprintf(&b, `<TR><TD COLSPAN="2"><B>%s</B></TD></TR>`, escape(string(n.Name)))
	for _, row := range n.Rows {
		fmt.Fprintf(&b, `<TR><TD COLSPAN="2" ALIGN="LEFT">%s</TD></TR>`, escape(row))
	}
	for i := 0; i < max(len(n.In), len(n.Out)); i++ {
		b.WriteString("<TR>")
		if i < len(n.In) {
			fmt.Fprintf(&b, `<TD PORT="%s" ALIGN="LEFT">%s</TD>`, inAnchor(n.In[i]), escape(string(n.In[i])))
		} else {
			b.WriteString(`<TD></TD>`)
		}
		if i < len(n.Out) {
			fmt.Fprintf(&b, `<TD PORT="%s" ALIGN="RIGHT">%s</TD>`, outAnchor(n.Out[i]), escape(string(n.Out[i])))
		} else {
			b.WriteString(`<TD></TD>`)
		}
		b.WriteString("</TR>")
	}
	b.WriteString("</TABLE>")
	return b.String()
}

// The two anchors one port name can be. A port may be both an input and an output of one
// step, since the two blocks of a manifest are separate and a name written in both is one
// brick reading and writing the same thing, so the side is part of the anchor and not only
// the name.
func inAnchor(p agk.Port) string  { return "in_" + string(p) }
func outAnchor(p agk.Port) string { return "out_" + string(p) }

// quote writes one DOT identifier as a quoted string, which is what lets a step name, a
// digest and a namespaced reference all travel unchanged.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// escape writes text inside an HTML-like label. An expression carries > and a digest
// carries neither, so the three that matter are the three markup reads.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
