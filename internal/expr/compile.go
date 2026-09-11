package expr

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types/ref"
)

// rootTypes is what the checker knows each root as. Nine of the ten are documents, so
// they are maps of strings to dyn and a selector into one is checked for shape and not
// for a key the engine could not know. The tenth is secrets, which is a map of strings to
// an opaque type: a value with no overload for concatenation, comparison against a string
// or conversion to one, so every path from a secret to a text position is a type error.
var rootTypes = map[Root]*cel.Type{
	RootWorkflow: cel.MapType(cel.StringType, cel.DynType),
	RootRun:      cel.MapType(cel.StringType, cel.DynType),
	RootTrigger:  cel.MapType(cel.StringType, cel.DynType),
	RootEvent:    cel.MapType(cel.StringType, cel.DynType),
	RootVars:     cel.MapType(cel.StringType, cel.DynType),
	RootInputs:   cel.MapType(cel.StringType, cel.DynType),
	RootSteps:    cel.MapType(cel.StringType, cel.DynType),
	RootItem:     cel.DynType,
	RootMatrix:   cel.MapType(cel.StringType, cel.DynType),
	RootSecrets:  cel.MapType(cel.StringType, secretType),
}

// environments is one compilation environment per position, built once. The environment
// is where the exposed-context table is enforced: a position declares the roots it
// exposes and nothing else, so a root it does not expose is an undeclared reference and
// the compiler is the one that says so. A guard is a check somebody removes; a missing
// declaration is a compile failure.
// They are built at first use and never again: an environment is what cel-go intends to
// be shared rather than made per expression, and building one costs the whole standard
// declaration set.
var environments = sync.OnceValues(buildEnvironments)

func buildEnvironments() (map[Scope]*cel.Env, error) {
	envs := make(map[Scope]*cel.Env, len(scopeRoots))
	for sc, roots := range scopeRoots {
		// The escape syntax is what makes the rewrite in escape.go legal input:
		// without it a backtick is unsupported syntax and inputs.in.count has no
		// spelling at all.
		opts := []cel.EnvOption{cel.EnableIdentifierEscapeSyntax()}
		for _, r := range roots {
			opts = append(opts, cel.Variable(r.String(), rootTypes[r]))
		}
		env, err := cel.NewEnv(opts...)
		if err != nil {
			return nil, fmt.Errorf("building the environment for %s: %w", sc, err)
		}
		envs[sc] = env
	}
	return envs, nil
}

// UndeclaredRoot is an expression reading a name the position does not expose. It
// carries the name as the expression writes it, so that a caller holding the workflow
// file can name the rule that was broken: a name that is not one of the ten roots at all,
// item where the step is not sharded per item, or a secret outside params and secrets.
// Which of those it is, is the language's to say and not this package's.
type UndeclaredRoot struct {
	// Name is the identifier the expression read, as it was written.
	Name string

	// Scope is the position the expression sits in.
	Scope Scope

	// Source is the expression, as the author wrote it, before any rewrite.
	Source string

	// Line and Column are 1 based and point into Source.
	Line, Column int
}

func (e *UndeclaredRoot) Error() string {
	return fmt.Sprintf("%s is not exposed to %s, which reads %s", e.Name, e.Scope, rootList(e.Scope.Roots()))
}

// Program is one compiled expression, bound to the position it was compiled for. It
// holds what it was compiled from, so that a refusal or a log line can show the
// expression the author wrote rather than the text the parser read.
type Program struct {
	scope   Scope
	source  string
	escaped string
	offsets []int
	roots   []Root
	out     *cel.Type
	prg     cel.Program
}

// Compile reads one expression, the text between ${{ and }} and not the braces, for the
// position it was written in.
//
// The order is fixed and each step answers one question: the rewrite makes the author's
// text parseable, the parse says whether it is an expression at all, the free
// identifiers say which roots it reads and whether the position exposes them, and the
// check says whether it type checks. An undeclared root is reported before a type error
// because a root the position does not expose makes every type below it meaningless.
func Compile(sc Scope, src string) (*Program, error) {
	envs, err := environments()
	if err != nil {
		return nil, err
	}
	env, ok := envs[sc]
	if !ok {
		return nil, fmt.Errorf("%s is not a position an expression is compiled for", sc)
	}

	escaped, offsets, err := Escape(src)
	if err != nil {
		return nil, err
	}

	p := &Program{scope: sc, source: src, escaped: escaped, offsets: offsets}

	parsed, issues := env.Parse(escaped)
	if issues != nil && issues.Err() != nil {
		return nil, p.issueError(issues)
	}

	for _, id := range freeIdentifiers(parsed) {
		r, known := ParseRoot(id.name)
		if !known || !sc.declares(r) {
			line, column := p.position(id.offset)
			return nil, &UndeclaredRoot{
				Name:   id.name,
				Scope:  sc,
				Source: src,
				Line:   line,
				Column: column,
			}
		}
		p.roots = appendRoot(p.roots, r)
	}
	slices.Sort(p.roots)

	checked, issues := env.Check(parsed)
	if issues != nil && issues.Err() != nil {
		return nil, p.issueError(issues)
	}
	p.out = checked.OutputType()

	prg, err := env.Program(checked)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	p.prg = prg
	return p, nil
}

// Roots names every root the expression reads, in the table's order. It is what lets a
// caller that knows more than a position hold an expression to a rule the position
// cannot state: item is exposed to a shard, and only a caller that knows the step's
// fan_out knows whether this shard is an item.
func (p *Program) Roots() []Root {
	out := make([]Root, len(p.roots))
	copy(out, p.roots)
	return out
}

// Source gives the expression as the author wrote it.
func (p *Program) Source() string { return p.source }

// Eval evaluates the expression against a context and gives the value back as the
// document value it is: a string, a number, a boolean, a list, a map, a timestamp, a
// duration, nothing at all, or a Secret where a secret fills the whole value.
func (p *Program) Eval(c Context) (any, error) {
	v, err := p.eval(c)
	if err != nil {
		return nil, err
	}
	return native(v)
}

// eval keeps the evaluated value in CEL's own terms, which is what a text position needs
// in order to convert it the way the language converts it.
func (p *Program) eval(c Context) (ref.Val, error) {
	v, _, err := p.prg.Eval(c.activation(p.scope))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.source, err)
	}
	return v, nil
}

// outputType is what the checker decided the expression evaluates to, or nil where it is
// dynamic. It is what lets a secret in a text position be refused while the workflow is
// being read rather than while a run is under way.
func (p *Program) outputType() *cel.Type { return p.out }

// issueError takes the compiler's first complaint and gives it a position in the
// author's text. The compiler's wording is kept: it is precise, and inventing a second
// vocabulary for a syntax error would only make two.
func (p *Program) issueError(issues *cel.Issues) error {
	errs := issues.Errors()
	if len(errs) == 0 {
		return fmt.Errorf("%s: %w", p.source, issues.Err())
	}
	first := errs[0]
	at := escapedOffset(p.escaped, first.Location.Line(), first.Location.Column())
	line, column := p.positionAt(at)
	return fmt.Errorf("%s: at line %d column %d: %s", p.source, line, column, first.Message)
}

// position turns a character offset in the text the parser read, which is how the parser
// keeps a position, into a line and a column in the text the author wrote.
func (p *Program) position(charOffset int) (line, column int) {
	if charOffset < 0 {
		return 1, 1
	}
	return p.positionAt(byteOffset(p.escaped, charOffset))
}

// positionAt turns a byte offset in the text the parser read into a line and a column in
// the text the author wrote. Both are 1 based, and a column counts characters and not
// bytes, which is what a person counts when they look at the line.
func (p *Program) positionAt(at int) (line, column int) {
	return lineColumn(p.source, sourceOffset(p.offsets, at))
}

// reference is one free identifier the expression reads, with where it reads it.
type reference struct {
	name   string
	offset int // character offset into the parsed text, or -1 where the parser kept none
}

// freeIdentifiers names every identifier the expression reads that it did not itself
// bind. A comprehension binds its iteration and accumulator variables, and those are not
// roots however they are spelled: a list read as [1].exists(item, item > 0) reads no item
// root, and reporting one would refuse an expression that reads nothing it may not.
func freeIdentifiers(a *cel.Ast) []reference {
	rep := a.NativeRep()
	info := rep.SourceInfo()
	bound := make(map[string]bool)
	var refs []reference
	celast.PostOrderVisit(rep.Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		switch e.Kind() {
		case celast.IdentKind:
			offset := -1
			if r, ok := info.GetOffsetRange(e.ID()); ok {
				offset = int(r.Start)
			}
			refs = append(refs, reference{name: e.AsIdent(), offset: offset})
		case celast.ComprehensionKind:
			c := e.AsComprehension()
			bound[c.IterVar()] = true
			if c.HasIterVar2() {
				bound[c.IterVar2()] = true
			}
			bound[c.AccuVar()] = true
		}
	}))
	free := refs[:0]
	for _, r := range refs {
		if !bound[r.name] {
			free = append(free, r)
		}
	}
	// In source order, so that an expression reading two names it may not read is
	// refused by the first one a person would see.
	slices.SortStableFunc(free, func(a, b reference) int { return cmp.Compare(a.offset, b.offset) })
	return free
}

// appendRoot adds a root once. An expression reading inputs twice reads one root.
func appendRoot(roots []Root, r Root) []Root {
	for _, have := range roots {
		if have == r {
			return roots
		}
	}
	return append(roots, r)
}

// escapedOffset turns a line and a 0 based character column, which is how the compiler
// reports a position, into a byte offset in the same text.
func escapedOffset(s string, line, column int) int {
	at := 0
	for l := 1; l < line; l++ {
		nl := strings.IndexByte(s[at:], '\n')
		if nl < 0 {
			return len(s)
		}
		at += nl + 1
	}
	for c := 0; c < column && at < len(s); c++ {
		_, size := utf8.DecodeRuneInString(s[at:])
		at += size
	}
	return at
}

// byteOffset turns a character offset into a byte offset in the same text.
func byteOffset(s string, chars int) int {
	at := 0
	for c := 0; c < chars && at < len(s); c++ {
		_, size := utf8.DecodeRuneInString(s[at:])
		at += size
	}
	return at
}

// lineColumn gives the 1 based line and character column of a byte offset.
func lineColumn(s string, at int) (line, column int) {
	if at > len(s) {
		at = len(s)
	}
	line = 1 + strings.Count(s[:at], "\n")
	start := strings.LastIndexByte(s[:at], '\n') + 1
	return line, 1 + utf8.RuneCountInString(s[start:at])
}
