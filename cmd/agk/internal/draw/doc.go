// Package draw writes a resolved workflow as DOT and as Mermaid, which is what agk graph
// is for: review inside a merge request.
//
// It draws from a *graph.Workflow, after Load and Check, and deliberately not from a
// *graph.Graph. Build needs the brick manifests, reading a manifest means pulling an image,
// and the person reading a merge request has no daemon. A drawing that needed one would be
// a drawing nobody could produce in the place it is wanted. That is the whole reason this
// is a package of its own: the rule is an import list rather than a convention, because
// nothing here can reach driver.
//
// What is drawn is what the file declares, resolved. One node per step. One edge per needs
// entry, anchored from the port it leaves by to the port it feeds, so that an edge in the
// picture is an edge in the file and not a line between two boxes. Workflow inputs and
// outputs are the boundary and are drawn dashed, because an output is a view of a step
// port and not a step. fan_out, merge and if appear as rows inside the node they belong
// to, since each is a property of one step and a badge beside a box is a thing a reader
// has to match up.
//
// Steps and ports are emitted in sorted order. That is not tidiness: two runs of agk graph
// over one file have to produce identical bytes, so that a diff of two drawings is a diff
// of the graph and a drawing can be committed beside the file it describes.
//
// # What this package refuses to be
//
// No layout engine, no colour, no theme and no SVG. DOT goes to dot and Mermaid goes to
// whatever renders Mermaid, and both of those are better at it than this would be. Nothing
// here reads a daemon, a store or a clock.
//
// # Layout
//
//	draw.go      the reading both writers share: the sorted steps, the rows of a node, the
//	             edges and the two ends of the boundary. It is one file rather than two
//	             readings because a drawing has to be the same graph in both languages, and
//	             two readings of one file are two things to keep in step
//	dot.go       DOT
//	mermaid.go   Mermaid, as flowchart LR
//	testdata/    one fixture and the two golden files, plus a test that both writers name
//	             every step, every port, every edge and both boundaries of it
package draw
