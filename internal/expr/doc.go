// Package expr is CEL behind a door: the exposed-context table as a set of compilation
// environments, and the two interpolation rules the language states.
//
// It is internal, and deliberately. cel-go drags antlr, protobuf, genproto and
// golang.org/x/exp behind it, and only the evaluator needs any of it: the vocabulary,
// the store, the brick contract and the driver must not pay for a parser they never
// call. The precedent is package schema, which isolates the JSON Schema implementation
// for the same reason and says so in its own doc.
//
// # One environment per position, not one environment with guards
//
// The exposed-context table is an environment table. Its "Available in" column is a
// Scope, and a Scope declares exactly the roots that position exposes and no others.
// Three of the fourteen rules then fall out of the compiler rather than out of checks a
// later refactor can forget: a root outside the ten is an undeclared reference, item
// outside fan_out: item is an undeclared reference, and secrets outside params and
// secrets is an undeclared reference. A guard is a check somebody removes; a missing
// declaration is a compile failure.
//
// The compiler's word for that failure is not the documentation's, so package graph
// translates it into the rule the corpus names. What lives here is the mechanism; what
// the refusal says is the language's, and the language belongs to graph.
//
// # What this package refuses to be
//
// It is not the workflow language. The ${{ }} scan, the rule that an expression filling
// a whole value keeps its type and an embedded one is converted to text, the roots that
// are exposed where: those are stated here as a Scope and a Template because that is
// where they are enforceable, but the wording of every refusal, the position it names
// and the decision about which Scope a given keyword sits in are graph's. Nothing here
// reads a file, a clock or a store, and nothing here knows what a step is.
//
// Secret is an opaque value that stringifies to nothing and refuses to be interpolated,
// so a secret reaching a text position is a refusal rather than a leak. PortMeta is
// count, empty and bytes and is the whole of what an expression may see of a port, which
// is what keeps the controller from loading a namespace's business data in order to
// schedule it.
//
// # One fact about cel-go that is not a fact about the workflow language
//
// The documentation's own canonical expression does not parse as written. ${{
// inputs.in.count > 0 }} appears in the docs and in the fixture corpus, and in is a CEL
// reserved word: cel-go answers with a syntax error at the dot. It is not an edge case,
// because in is the input port the short form of an edge feeds, which makes this the
// most common expression in the language. The same pass has a second case: a port name
// may carry a hyphen, and inputs.my-port.count parses as a subtraction and reports an
// undeclared reference to port, which is a misleading error rather than an honest one.
//
// The fix is CEL's own sanctioned escape. Under cel.EnableIdentifierEscapeSyntax, a
// selector segment that is a reserved word or is not a bare CEL identifier is rewritten
// lexically into the backtick form, never inside a string literal and never before a
// call parenthesis, with an offset map kept so that a position still points into the
// text the author wrote. It is one small pass with its own tests, it runs before
// anything else here works, and it lives on this side of the door because it is a fact
// about the parser and not about the language.
package expr
