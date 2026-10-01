package console

import (
	"cmp"
	"slices"
	"strings"
)

// The graph drawn top to bottom, in a pane taller than it is wide, the run's pane beside the runs
// and the log among them: each layer a row of boxes, each edge leaving a box by its foot, named at
// the port it leaves, and arriving on the top of the box it feeds with an arrowhead. An edge that
// moves sideways turns in a row of its own between the two layers, and one that skips a layer runs
// down a corridor on the right, so that no two lines run along the same cells where the graph
// allows it.

// drawingFor is the graph drawn for a pane: left to right where the pane is wider than tall, as
// the eye measures it, a cell being about twice as tall as it is wide, and top to bottom otherwise;
// and the other way where only the other fits in the rows and columns room leaves it.
func (m Model) drawingFor(g *flowGraph, width, height int) *drawing {
	return m.drawingIn(g, width, height, width, height)
}

// drawingIn is the graph drawn for a pane of the shape given, in the room given within it.
func (m Model) drawingIn(g *flowGraph, width, height, roomW, roomH int) *drawing {
	across, down := m.layout, m.layoutDown
	if width < 2*height {
		across, down = down, across
	}
	d := across(g)
	if d.canvas.w > roomW || d.canvas.h > roomH {
		if other := down(g); other.canvas.w <= roomW && other.canvas.h <= roomH {
			return other
		}
	}
	return d
}

// layoutDown places the graph top to bottom, each layer a row, and draws it.
func (m Model) layoutDown(g *flowGraph) *drawing {
	layers, nodes, inputNames := m.arranged(g)
	edges := m.drawnEdges(g, inputNames)
	tall := func(n *node) int {
		if n.pill {
			return 1
		}
		return boxHeight
	}

	// Each edge leaves its box by a column of its own, spaced by the label written beside it, and
	// a box is widened for its labels to fit beneath it.
	leaving := map[string][]int{}
	arriving := map[string][]int{}
	for i, e := range edges {
		leaving[e.from] = append(leaving[e.from], i)
		arriving[e.to] = append(arriving[e.to], i)
	}
	for name, list := range leaving {
		if n := nodes[name]; !n.pill {
			need := 3
			for _, i := range list {
				need += len([]rune(edges[i].label)) + 2
			}
			n.w = max(n.w, need)
		}
	}
	const gapX = 3
	widest := 0
	for _, layer := range layers {
		w := 0
		for _, n := range layer {
			w += n.w + gapX
		}
		widest = max(widest, w-gapX)
	}
	for _, layer := range layers {
		w := -gapX
		for _, n := range layer {
			w += n.w + gapX
		}
		x := 1 + (widest-w)/2
		for _, n := range layer {
			n.x = x
			x += n.w + gapX
		}
	}
	exitX := map[int]int{}
	for name, list := range leaving {
		n := nodes[name]
		x := n.x + 2
		for _, i := range list {
			if n.pill {
				exitX[i] = n.x + n.w/2
				continue
			}
			exitX[i] = x
			x += len([]rune(edges[i].label)) + 2
		}
	}
	// A box's arrivals are spread along its top, in the order of the columns they come from, so
	// that two of them cross above it no more than they have to.
	// An edge whose column falls on the box it feeds arrives straight down it, where that keeps the
	// arrivals in their order; a long edge, which comes from the corridor on the right, arrives as
	// far right as it can.
	entryX := map[int]int{}
	for name, list := range arriving {
		n := nodes[name]
		from := func(i int) int {
			if nodes[edges[i].from].layer+1 < n.layer {
				return n.x + n.w
			}
			return exitX[i]
		}
		slices.SortStableFunc(list, func(a, b int) int { return cmp.Compare(from(a), from(b)) })
		last := n.x
		for k, i := range list {
			if n.pill {
				entryX[i] = n.x + n.w/2
				continue
			}
			x := n.x + 1 + (k+1)*(n.w-2)/(len(list)+1)
			if want := from(i); want > last && want > n.x && want < n.x+n.w-1 {
				x = want
			}
			// What is left of the box's top for the arrivals after this one.
			x = min(max(x, last+2), n.x+n.w-2-2*(len(list)-1-k))
			entryX[i], last = x, x
		}
	}

	// The rows between two layers: the labels', a lane for each edge that turns there, and the
	// arrowheads'. A long edge turns in the gap below where it leaves and in the gap above where it
	// arrives, and runs down a corridor of its own on the right between the two.
	type turn struct {
		edge int
		back bool
		x    int
	}
	turns := make([][]turn, len(layers))
	long := 0
	corridorOf := map[int]int{}
	for i, e := range edges {
		from, to := nodes[e.from], nodes[e.to]
		switch {
		case to.layer == from.layer+1 && exitX[i] == entryX[i]:
		case to.layer == from.layer+1:
			turns[from.layer] = append(turns[from.layer], turn{i, false, exitX[i]})
		default:
			corridorOf[i] = long
			long++
			turns[from.layer] = append(turns[from.layer], turn{i, false, exitX[i]})
			turns[to.layer-1] = append(turns[to.layer-1], turn{i, true, entryX[i]})
		}
	}
	// In a gap, the edges turning right turn first from the furthest right, and those turning left
	// after them from the furthest left, so that a line turning crosses no other one still on its
	// way down; an edge coming back from the corridor turns last.
	laneOf := map[[2]int]int{}
	rightward := func(t turn) bool {
		return !t.back && (nodes[edges[t.edge].to].layer > nodes[edges[t.edge].from].layer+1 || entryX[t.edge] > exitX[t.edge])
	}
	for gap, list := range turns {
		slices.SortStableFunc(list, func(a, b turn) int {
			switch {
			case a.back != b.back:
				if a.back {
					return 1
				}
				return -1
			case rightward(a) != rightward(b):
				if rightward(a) {
					return -1
				}
				return 1
			case rightward(a):
				return cmp.Compare(b.x, a.x)
			}
			return cmp.Compare(a.x, b.x)
		})
		for lane, t := range list {
			key := [2]int{t.edge, 0}
			if t.back {
				key[1] = 1
			}
			laneOf[key] = lane
		}
		_ = gap
	}
	y := 1
	gapAt := make([]int, len(layers))
	for i, layer := range layers {
		if len(layer) == 0 {
			gapAt[i] = y
			continue
		}
		h := 0
		for _, n := range layer {
			h = max(h, tall(n))
		}
		for _, n := range layer {
			n.y = y + (h-tall(n))/2
		}
		gapAt[i] = y + h
		y += h + 1 + len(turns[i]) + 1
	}
	c := newCanvas(1+widest+2+long+1, y)
	corridor := 1 + widest + 2

	for i, e := range edges {
		from, to := nodes[e.from], nodes[e.to]
		ex, tx := exitX[i], entryX[i]
		start := from.y + tall(from)
		end := to.y - 1
		r, dashed := portRole(e.port), e.port == "rejected"
		lane := gapAt[from.layer] + 1 + laneOf[[2]int{i, 0}]
		switch {
		case to.layer == from.layer+1 && ex == tx:
			c.path(r, dashed, [2]int{ex, start}, [2]int{tx, end})
		case to.layer == from.layer+1:
			c.path(r, dashed, [2]int{ex, start}, [2]int{ex, lane}, [2]int{tx, lane}, [2]int{tx, end})
		default:
			cx := corridor + corridorOf[i]
			back := gapAt[to.layer-1] + 1 + laneOf[[2]int{i, 1}]
			c.path(r, dashed, [2]int{ex, start}, [2]int{ex, lane}, [2]int{cx, lane}, [2]int{cx, back}, [2]int{tx, back}, [2]int{tx, end})
		}
		c.write(tx, end, "▾", r)
		if e.label != "" {
			c.write(ex+1, start, e.label, r)
		}
	}
	chosen := m.graphStep()
	for _, n := range nodes {
		if n.pill {
			name := strings.TrimPrefix(strings.TrimPrefix(n.name, "input:"), "output:")
			c.write(n.x, n.y, cellOf("( "+name+" )", n.w), muted)
			continue
		}
		c.box(n.x, n.y, n.w, boxHeight, n.name == chosen)
		m.boxText(c, g, n)
	}
	return &drawing{nodes: nodes, canvas: c}
}
