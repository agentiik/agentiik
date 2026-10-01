package console

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// The graph drawn as the web console draws it, in the terminal's own characters: each step a box
// with rounded corners, each edge a line of ─ and │ whose turns are rounded and which ends in an
// arrowhead where it arrives, named at the port it leaves with the items it published. Steps sit in
// layers in an order the graph runs in, left to right, each layer ordered against the one before
// to keep crossings few, ties broken by name; the workflow's inputs and outputs are pills at either
// edge. A drawing larger than its pane pans to keep the step chosen in view, and never shrinks.

// Directions a line leaves a cell by, which together choose the character drawn there.
const (
	toLeft uint8 = 1 << iota
	toRight
	toUp
	toDown
)

// joints are the characters a cell's directions draw: straight, rounded at a turn, a tee where
// lines meet, and a cross where they pass.
var joints = map[uint8]rune{
	toLeft: '─', toRight: '─', toLeft | toRight: '─',
	toUp: '│', toDown: '│', toUp | toDown: '│',
	toRight | toDown: '╭', toLeft | toDown: '╮', toUp | toRight: '╰', toUp | toLeft: '╯',
	toLeft | toRight | toDown: '┬', toLeft | toRight | toUp: '┴', toUp | toDown | toRight: '├', toUp | toDown | toLeft: '┤',
	toLeft | toRight | toUp | toDown: '┼',
}

type canvasCell struct {
	text   rune
	dirs   uint8
	role   role
	dashed bool
}

// canvas is the drawing before it is cut to its pane.
type canvas struct {
	w, h  int
	cells [][]canvasCell
}

func newCanvas(w, h int) *canvas {
	c := &canvas{w: w, h: h, cells: make([][]canvasCell, h)}
	for y := range c.cells {
		c.cells[y] = make([]canvasCell, w)
	}
	return c
}

func (c *canvas) at(x, y int) *canvasCell {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return nil
	}
	return &c.cells[y][x]
}

// joint adds directions to a cell of a line, in the line's colour: a failure's red over a
// refusal's amber over data, where two lines share a cell.
func (c *canvas) joint(x, y int, dirs uint8, r role, dashed bool) {
	cell := c.at(x, y)
	if cell == nil {
		return
	}
	cell.dirs |= dirs
	if cell.dirs == dirs || rank(r) > rank(cell.role) {
		cell.role, cell.dashed = r, dashed
	}
}

func rank(r role) int {
	switch r {
	case failedText:
		return 3
	case waitingText:
		return 2
	case muted:
		return 1
	}
	return 0
}

func (c *canvas) horizontal(x0, x1, y int, r role, dashed bool) {
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	for x := x0; x <= x1; x++ {
		var d uint8
		if x > x0 {
			d |= toLeft
		}
		if x < x1 {
			d |= toRight
		}
		if x0 == x1 {
			d = toLeft | toRight
		}
		c.joint(x, y, d, r, dashed)
	}
}

func (c *canvas) vertical(x, y0, y1 int, r role, dashed bool) {
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	for y := y0; y <= y1; y++ {
		var d uint8
		if y > y0 {
			d |= toUp
		}
		if y < y1 {
			d |= toDown
		}
		c.joint(x, y, d, r, dashed)
	}
}

// path draws a line through points, each segment straight, the turns joined.
func (c *canvas) path(r role, dashed bool, points ...[2]int) {
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if a[1] == b[1] {
			c.horizontal(a[0], b[0], a[1], r, dashed)
		} else {
			c.vertical(a[0], a[1], b[1], r, dashed)
		}
	}
}

func (c *canvas) write(x, y int, s string, r role) {
	for _, ch := range s {
		if cell := c.at(x, y); cell != nil {
			cell.text, cell.role = ch, r
		}
		x++
	}
}

// box draws a step's frame, rounded, or heavier where it is the one chosen.
func (c *canvas) box(x, y, w, h int, chosen bool) {
	r, corners, across, down := muted, "╭╮╰╯", "─", "│"
	if chosen {
		r, corners, across, down = strong, "┏┓┗┛", "━", "┃"
	}
	cs := []rune(corners)
	c.write(x, y, string(cs[0])+strings.Repeat(across, w-2)+string(cs[1]), r)
	for i := 1; i < h-1; i++ {
		c.write(x, y+i, down+strings.Repeat(" ", w-2)+down, r)
	}
	c.write(x, y+h-1, string(cs[2])+strings.Repeat(across, w-2)+string(cs[3]), r)
}

// node is a step or a pill placed in a layer: where it sits and how large it is.
type node struct {
	name        string
	pill        bool
	layer, slot int
	x, y, w     int
}

// drawing is a graph laid out: its nodes by name, the pills of the inputs and outputs by
// "input:" and "output:" and their names, and the canvas drawn.
type drawing struct {
	nodes  map[string]*node
	canvas *canvas
}

// boxHeight is a step's box: a line for its glyph and name, one for its state and duration, and
// one for a fan-out's shards or what merge and when say, inside its frame.
const boxHeight = 5

// arranged are the graph's steps, inputs and outputs in layers, each layer ordered against the one
// before it, as both directions draw them: the inputs' pills first, each step one past the furthest
// it needs, the outputs' pills last.
func (m Model) arranged(g *flowGraph) (layers [][]*node, nodes map[string]*node, inputNames []string) {
	layerOf := map[string]int{}
	// The inputs' pills take the first layer, so a step fed by nothing else is in the second, and
	// each step one past the furthest it needs, since it runs once what it needs has published.
	for _, step := range g.Order {
		l := 1
		for _, n := range g.Steps[step].Needs {
			l = max(l, layerOf[n.Step]+1)
		}
		layerOf[step] = l
	}
	last := 0
	for _, l := range layerOf {
		last = max(last, l)
	}
	layers = make([][]*node, last+2)
	nodes = map[string]*node{}
	boxWidth := 18
	for _, step := range g.Order {
		boxWidth = max(boxWidth, len([]rune(step))+6)
	}
	boxWidth = min(boxWidth, 30)
	for _, step := range g.Order {
		n := &node{name: step, layer: layerOf[step], w: boxWidth}
		nodes[step] = n
		layers[n.layer] = append(layers[n.layer], n)
	}
	var outputNames []string
	for _, fed := range g.Order {
		for _, e := range g.Steps[fed].Inputs {
			if name := inputOf(e); name != "" && !slices.Contains(inputNames, name) {
				inputNames = append(inputNames, name)
			}
		}
	}
	slices.Sort(inputNames)
	for name := range g.Outputs {
		outputNames = append(outputNames, name)
	}
	slices.Sort(outputNames)
	for _, name := range inputNames {
		n := &node{name: "input:" + name, pill: true, layer: 0, w: len([]rune(name)) + 4}
		nodes[n.name] = n
		layers[0] = append(layers[0], n)
	}
	for _, name := range outputNames {
		n := &node{name: "output:" + name, pill: true, layer: last + 1, w: len([]rune(name)) + 4}
		nodes[n.name] = n
		layers[last+1] = append(layers[last+1], n)
	}

	// What feeds each node, to order a layer against the one before it.
	feeds := map[string][]string{}
	for _, step := range g.Order {
		for _, n := range g.Steps[step].Needs {
			feeds[step] = append(feeds[step], n.Step)
		}
		for _, e := range g.Steps[step].Inputs {
			if name := inputOf(e); name != "" {
				feeds[step] = append(feeds[step], "input:"+name)
			}
		}
	}
	for name, o := range g.Outputs {
		feeds["output:"+name] = []string{o.From.Step}
	}
	for i, layer := range layers {
		slices.SortStableFunc(layer, func(a, b *node) int { return cmp.Compare(a.name, b.name) })
		if i > 0 {
			centre := func(n *node) float64 {
				sum, count := 0.0, 0
				for _, f := range feeds[n.name] {
					if from, ok := nodes[f]; ok {
						sum += float64(from.slot)
						count++
					}
				}
				if count == 0 {
					return float64(n.slot)
				}
				return sum / float64(count)
			}
			for j, n := range layer {
				n.slot = j
			}
			slices.SortStableFunc(layer, func(a, b *node) int { return cmp.Compare(centre(a), centre(b)) })
		}
		for j, n := range layer {
			n.slot = j
		}
	}
	return layers, nodes, inputNames
}

// layout places the graph left to right, each layer a column, and draws it.
func (m Model) layout(g *flowGraph) *drawing {
	layers, nodes, inputNames := m.arranged(g)
	rows := 0
	for _, layer := range layers {
		rows = max(rows, len(layer))
		for _, n := range layer {
			n.y = 1 + n.slot*(boxHeight+1)
		}
	}
	exitRow := func(n *node, i int) int {
		if n.pill {
			return n.y + boxHeight/2
		}
		return n.y + 1 + i%(boxHeight-2)
	}

	edges := m.drawnEdges(g, inputNames)
	// A node's edges arrive in the order of the heights they leave from, so that two of them
	// cross before it no more than they have to.
	for i := range edges {
		edges[i].ey = exitRow(nodes[edges[i].from], edges[i].exit)
	}
	arriving := map[string][]int{}
	for i, e := range edges {
		arriving[e.to] = append(arriving[e.to], i)
	}
	for to, list := range arriving {
		slices.SortStableFunc(list, func(a, b int) int { return cmp.Compare(edges[a].ey, edges[b].ey) })
		for k, i := range list {
			edges[i].ty = exitRow(nodes[to], k)
		}
	}

	// Each edge that turns between two layers turns in a lane of its own. One arriving on a row
	// turns to the right of every one leaving on that row, or the two would run along the same
	// cells: the lanes of a gap are ordered so, where the edges allow it.
	label := make([]int, len(layers))
	type turn struct {
		edge        int
		left, right int // the rows its horizontals run on before and after it turns, or -1
		back        bool
	}
	turns := make([][]turn, len(layers))
	long := 0
	for i := range edges {
		e := &edges[i]
		from, to := nodes[e.from], nodes[e.to]
		label[from.layer] = max(label[from.layer], len([]rune(e.label))+2)
		switch {
		case to.layer == from.layer+1 && e.ey == e.ty:
		case to.layer == from.layer+1:
			turns[from.layer] = append(turns[from.layer], turn{i, e.ey, e.ty, false})
		default:
			e.corridor = long
			long++
			turns[from.layer] = append(turns[from.layer], turn{i, e.ey, -1, false})
			turns[to.layer-1] = append(turns[to.layer-1], turn{i, -1, e.ty, true})
		}
	}
	lanes := make([]int, len(layers))
	for gap, list := range turns {
		placed := make([]bool, len(list))
		for lane := range list {
			// The first edge not yet placed that no unplaced edge has to precede.
			pick := -1
			for a := range list {
				if placed[a] {
					continue
				}
				free := true
				for b := range list {
					if !placed[b] && b != a && list[b].left >= 0 && list[b].left == list[a].right {
						free = false
						break
					}
				}
				if free {
					pick = a
					break
				}
			}
			if pick < 0 {
				// The edges ask for each other first, and cross whatever the order.
				pick = slices.Index(placed, false)
			}
			placed[pick] = true
			if list[pick].back {
				edges[list[pick].edge].backLane = lane
			} else {
				edges[list[pick].edge].lane = lane
			}
		}
		lanes[gap] = len(list)
	}

	x := 1
	gapAt := make([]int, len(layers))
	for i, layer := range layers {
		if len(layer) == 0 {
			// A workflow with no input leaves the inputs' column empty, and it takes no room.
			gapAt[i] = x
			continue
		}
		w := 0
		for _, n := range layer {
			w = max(w, n.w)
		}
		for _, n := range layer {
			n.x = x + (w-n.w)/2
		}
		gapAt[i] = x + w
		x += w + label[i] + lanes[i] + 3
	}
	c := newCanvas(x+1, 1+rows*(boxHeight+1)+long+1)
	corridor := 1 + rows*(boxHeight+1)

	for _, e := range edges {
		from, to := nodes[e.from], nodes[e.to]
		ey, ty := e.ey, e.ty
		start := from.x + from.w
		laneX := gapAt[from.layer] + label[from.layer] + e.lane
		end := to.x - 2
		r, dashed := portRole(e.port), e.port == "rejected"
		if to.layer == from.layer+1 {
			if ey == ty {
				c.path(r, dashed, [2]int{start, ey}, [2]int{end, ty})
			} else {
				c.path(r, dashed, [2]int{start, ey}, [2]int{laneX, ey}, [2]int{laneX, ty}, [2]int{end, ty})
			}
		} else {
			back := gapAt[to.layer-1] + label[to.layer-1] + e.backLane
			cy := corridor + e.corridor
			c.path(r, dashed, [2]int{start, ey}, [2]int{laneX, ey}, [2]int{laneX, cy}, [2]int{back, cy}, [2]int{back, ty}, [2]int{end, ty})
		}
		c.write(to.x-1, ty, "▸", r)
		if e.label != "" {
			c.write(start+1, ey, e.label, r)
		}
	}
	chosen := m.graphStep()
	for _, n := range nodes {
		if n.pill {
			name := strings.TrimPrefix(strings.TrimPrefix(n.name, "input:"), "output:")
			c.write(n.x, n.y+boxHeight/2, "( "+name+" )", muted)
			continue
		}
		c.box(n.x, n.y, n.w, boxHeight, n.name == chosen)
		m.boxText(c, g, n)
	}
	return &drawing{nodes: nodes, canvas: c}
}

// boxText writes a step inside its frame: its state's glyph and its name, its state and duration,
// and a fan-out's shards and their count, or what merge and when say of it.
func (m Model) boxText(c *canvas, g *flowGraph, n *node) {
	room := n.w - 4
	verdict, _, ok := m.stateOf(n.name)
	glyph, r := "○", quiet
	if ok {
		glyph, r = m.mark(verdict == agk.VerdictRunning), verdictRole(verdict)
		if verdict != agk.VerdictRunning {
			glyph = "●"
		}
	}
	c.write(n.x+2, n.y+1, glyph, r)
	c.write(n.x+4, n.y+1, cellOf(n.name, room-2), strong)
	if ok {
		state := verdict.String()
		if s := summaryOf(m.run, n.name); s != nil {
			state += "  " + took(s.StartedAt, s.FinishedAt, m.o.Now())
		}
		c.write(n.x+2, n.y+2, cellOf(state, room), muted)
	}
	s := g.Steps[n.name]
	third := ""
	switch {
	case s.Merge != nil:
		if text, isText := s.Merge.(string); isText {
			third = "merge: " + text
		}
	case len(s.When) > 0:
		third = "when: " + strings.Join(s.When, ", ")
	}
	if m.run != nil {
		tasks := lastAttempts(m.run, n.name)
		if of := shardsOf(tasks); of > 1 {
			count := fmt.Sprintf(" %d/%d", doneOf(tasks), of)
			x := n.x + 2
			for _, cell := range shardCells(tasks, of, room-len(count)) {
				c.write(x, n.y+3, cell.text, cell.role)
				x++
			}
			c.write(x, n.y+3, count, muted)
			return
		}
	}
	if third != "" {
		c.write(n.x+2, n.y+3, cellOf(third, room), muted)
	}
}

// drawnEdge is an edge as it is drawn: from a node's port to another node, which row of each it
// leaves and reaches, its label, and the lanes and corridor it turns in.
type drawnEdge struct {
	from, to, port, label    string
	exit                     int
	ey, ty                   int
	lane, backLane, corridor int
}

// drawnEdges are the graph's edges, a workflow input's to the steps it feeds and each step's to
// the steps and outputs it feeds, labelled with the port they leave and the items it published.
func (m Model) drawnEdges(g *flowGraph, inputs []string) []drawnEdge {
	var out []drawnEdge
	for _, name := range inputs {
		for _, step := range g.Order {
			for _, e := range g.Steps[step].Inputs {
				if inputOf(e) == name {
					out = append(out, drawnEdge{from: "input:" + name, to: step})
					break
				}
			}
		}
	}
	for _, step := range g.Order {
		ports := map[string]int{}
		for _, e := range g.edgesFrom(step) {
			if _, ok := ports[e.port]; !ok {
				ports[e.port] = len(ports)
			}
			to := e.to
			if to == "" {
				to = "output:" + e.as
			}
			label := e.port
			if m.run != nil {
				if s := summaryOf(m.run, step); s != nil {
					if env, published := s.Ports[agk.Port(e.port)]; published {
						label += fmt.Sprintf(" %d", env.Items)
					}
				}
			}
			out = append(out, drawnEdge{from: step, to: to, port: e.port, label: label, exit: ports[e.port]})
		}
	}
	return out
}

// drawnLines is the drawing cut to its pane, panned so that the step chosen is in view: centred
// on it where the drawing is larger than the pane, and from its corner otherwise.
func (m Model) drawnLines(t theme, d *drawing, width, height int) []string {
	c := d.canvas
	ox, oy := 0, 0
	if n, ok := d.nodes[m.graphStep()]; ok {
		if c.w > width {
			ox = min(max(0, n.x+n.w/2-width/2), c.w-width)
		}
		if c.h > height {
			oy = min(max(0, n.y+boxHeight/2-height/2), c.h-height)
		}
	}
	// Each box is clicked where the window shows it.
	for step, n := range d.nodes {
		for y := max(n.y, oy); y < min(n.y+boxHeight, oy+height); y++ {
			if x0, x1 := max(n.x, ox)-ox, min(n.x+n.w, ox+width)-ox; x0 < x1 {
				t.pickAt(y-oy, x0, x1, "step", step)
			}
		}
	}
	var lines []string
	for y := oy; y < min(c.h, oy+height); y++ {
		var parts []part
		for x := ox; x < min(c.w, ox+width); x++ {
			cell := c.cells[y][x]
			ch := cell.text
			if ch == 0 {
				ch = ' '
				if cell.dirs != 0 {
					ch = joints[cell.dirs]
					if cell.dashed && ch == '─' {
						ch = '┄'
					}
					if cell.dashed && ch == '│' {
						ch = '┆'
					}
				}
			}
			if n := len(parts); n > 0 && parts[n-1].role == cell.role {
				parts[n-1].text += string(ch)
				continue
			}
			parts = append(parts, part{cell.role, string(ch)})
		}
		lines = append(lines, t.line(false, width, parts...))
	}
	return lines
}
