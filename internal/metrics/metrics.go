// Package metrics keeps the counters, histograms and gauges a server program exports, and writes
// them in the Prometheus text format for whoever is allowed to scrape them.
//
// # Why the Prometheus text format, written here
//
// The format is a few lines of text per series, which the standard library writes in a page, and it
// is what every scraper reads: Prometheus itself, and the OpenTelemetry Collector through its
// Prometheus receiver, which is how an installation that sends its traces as OpenTelemetry sends its
// metrics the same way. A client library would bring a dependency, a global registry and a set of
// process metrics nobody here asked for, to write the same lines.
//
// # Bounded
//
// A label taking the name of a namespace, a workflow or a brick takes whatever tenants write, so a
// family keeps at most Registry.Limit label sets. An observation that would add one more is counted
// under the label set whose every value is Other instead, which no namespace, workflow, brick or
// pool can be named since none of their grammars lets a name begin with an underscore, and
// agentiik_metrics_folded_total says for which family it happened. The totals stay right, a sum
// over a family still counts everything, and what the scraper stores is bounded however many names
// the tenants invent.
//
// A gauge is read when it is scraped rather than kept, so it holds what exists at that moment and
// nothing that has gone: it needs no bound of this kind.
package metrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// ContentType is what a scrape is answered with: the text format, version 0.0.4.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Other is the value every label takes in the label set an observation is folded into once its
// family holds Limit label sets.
const Other = "_other"

// DefaultLimit is how many label sets a family keeps where Registry.Limit is zero.
//
// A thousand label sets of a histogram with thirteen buckets is about sixteen thousand series, which
// a single Prometheus holds without anybody noticing, and a thousand bricks, or a thousand workflows
// finishing between two restarts of the controller, is more than a v0.2.0 installation is expected
// to run. One that does sees Other in its dashboards, and agentiik_metrics_folded_total names the
// family, rather than a scraper that fell over.
const DefaultLimit = 1000

// Registry is every family one program exports.
type Registry struct {
	// Limit is how many label sets one counter or histogram keeps, and DefaultLimit where zero.
	Limit int

	// Trouble hears of a gauge that could not be read, which a scrape leaves out rather than
	// failing: the counters are worth answering on their own. Nil says nothing.
	Trouble func(families []string, err error)

	mu         sync.Mutex
	families   []family
	names      map[string]bool
	collectors []collector
	folded     *Counter
}

// family is one metric, as it is written.
type family interface {
	write(w *bufio.Writer)
}

// collector reads gauges at every scrape.
type collector struct {
	families []Desc
	read     func(ctx context.Context, g *Gauges) error
}

// Desc names a gauge family read at scrape.
type Desc struct {
	Name, Help string
	Labels     []string
}

// NewRegistry is a registry holding agentiik_metrics_folded_total and nothing else yet.
func NewRegistry() *Registry {
	r := &Registry{names: map[string]bool{}}
	r.folded = r.Counter("agentiik_metrics_folded_total",
		"Observations counted under the label set _other because their family already held as many label sets as it keeps.",
		"metric")
	return r
}

// validName is a metric or label name the format accepts, without the colon, which only recording
// rules use.
var validName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// register claims a name, and panics on one that is taken or malformed: both are a mistake in the
// program, found the first time it starts.
func (r *Registry) register(name string, labels []string, f family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claim(name, labels)
	r.families = append(r.families, f)
}

func (r *Registry) claim(name string, labels []string) {
	if !validName.MatchString(name) || r.names[name] {
		panic(fmt.Sprintf("metrics: %q is taken or is not a metric name", name))
	}
	for _, l := range labels {
		if !validName.MatchString(l) || strings.HasPrefix(l, "__") || l == "le" {
			panic(fmt.Sprintf("metrics: %q is not a label %s may carry", l, name))
		}
	}
	r.names[name] = true
}

func (r *Registry) limit() int {
	if r.Limit > 0 {
		return r.Limit
	}
	return DefaultLimit
}

// Gauges registers families read at every scrape by read, which sets their values through g.
//
// Read at the scrape rather than kept, so that a gauge says what is true when it is asked and a
// runner that left or a pool that was deleted is simply not there. An error leaves every family
// this read sets out of the answer, and is said through Trouble.
func (r *Registry) Gauges(read func(ctx context.Context, g *Gauges) error, families ...Desc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range families {
		r.claim(d.Name, d.Labels)
	}
	r.collectors = append(r.collectors, collector{families: families, read: read})
}

// Gauges is what one read sets.
type Gauges struct {
	descs  map[string]Desc
	values map[string]map[string]float64
}

// Set gives the gauge name the value v for the label values given, in the order its Desc names
// the labels. A name the read was not registered for, or the wrong number of values, is a mistake
// in the program and panics.
func (g *Gauges) Set(name string, v float64, values ...string) {
	d, ok := g.descs[name]
	if !ok || len(values) != len(d.Labels) {
		panic(fmt.Sprintf("metrics: %s is not a gauge this read sets with %d labels", name, len(values)))
	}
	if g.values[name] == nil {
		g.values[name] = map[string]float64{}
	}
	g.values[name][key(values)] = v
}

// Counter is a family of counters that only go up.
type Counter struct {
	r          *Registry
	name, help string
	labels     []string
	mu         sync.Mutex
	series     map[string]float64
}

// Counter registers a counter family with the labels named.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	c := &Counter{r: r, name: name, help: help, labels: labels, series: map[string]float64{}}
	r.register(name, labels, c)
	return c
}

// Add adds n, which is never negative, for the label values given in the order the family names
// its labels.
func (c *Counter) Add(n float64, values ...string) {
	if n < 0 || math.IsNaN(n) {
		panic(fmt.Sprintf("metrics: %s only goes up, and was given %v", c.name, n))
	}
	c.mu.Lock()
	k, folded := c.r.slot(c.name, c.labels, values, len(c.series), func(k string) bool { _, ok := c.series[k]; return ok })
	c.series[k] += n
	c.mu.Unlock()
	if folded {
		c.r.folded.Add(1, c.name)
	}
}

// Inc adds one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Histogram is a family of distributions over buckets fixed when it is registered.
type Histogram struct {
	r          *Registry
	name, help string
	labels     []string
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*distribution
}

// distribution is one label set of a histogram: a count per bucket, not yet cumulated, the sum and
// the count.
type distribution struct {
	counts []uint64
	sum    float64
	count  uint64
}

// Histogram registers a histogram family over buckets, which are upper bounds in increasing order;
// the bucket +Inf is always added.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	if !slices.IsSorted(buckets) || len(buckets) == 0 {
		panic(fmt.Sprintf("metrics: the buckets of %s are not in increasing order", name))
	}
	h := &Histogram{r: r, name: name, help: help, labels: labels, buckets: buckets, series: map[string]*distribution{}}
	r.register(name, labels, h)
	return h
}

// Observe counts v for the label values given.
func (h *Histogram) Observe(v float64, values ...string) {
	if math.IsNaN(v) {
		return
	}
	h.mu.Lock()
	k, folded := h.r.slot(h.name, h.labels, values, len(h.series), func(k string) bool { _, ok := h.series[k]; return ok })
	d := h.series[k]
	if d == nil {
		d = &distribution{counts: make([]uint64, len(h.buckets)+1)}
		h.series[k] = d
	}
	i, _ := slices.BinarySearch(h.buckets, v)
	d.counts[i]++
	d.sum += v
	d.count++
	h.mu.Unlock()
	if folded {
		h.r.folded.Add(1, h.name)
	}
}

// slot is the key an observation is counted under: its own label set, or the one of Other where
// the family holds Limit label sets and this would be one more. It panics on the wrong number of
// values, which is a mistake in the program.
func (r *Registry) slot(name string, labels, values []string, held int, has func(string) bool) (string, bool) {
	if len(values) != len(labels) {
		panic(fmt.Sprintf("metrics: %s carries %d labels and was given %d values", name, len(labels), len(values)))
	}
	k := key(values)
	if has(k) || held < r.limit() {
		return k, false
	}
	other := make([]string, len(values))
	for i := range other {
		other[i] = Other
	}
	return key(other), true
}

// key is a label set as one string, each value quoted so that no two label sets share one however
// their values are spelled.
func key(values []string) string {
	var b strings.Builder
	for _, v := range values {
		b.WriteString(strconv.Quote(v))
	}
	return b.String()
}

// unkey is the label values a key was made of.
func unkey(k string) []string {
	var values []string
	for k != "" {
		v, err := strconv.QuotedPrefix(k)
		if err != nil {
			panic("metrics: a key this package did not make")
		}
		unquoted, _ := strconv.Unquote(v)
		values = append(values, unquoted)
		k = k[len(v):]
	}
	return values
}

// WriteTo writes every family in the text format: the counters and histograms as they stand, and
// the gauges as their reads find them now.
func (r *Registry) WriteTo(ctx context.Context, out io.Writer) error {
	r.mu.Lock()
	families := slices.Clone(r.families)
	collectors := slices.Clone(r.collectors)
	r.mu.Unlock()

	w := bufio.NewWriter(out)
	for _, f := range families {
		f.write(w)
	}
	for _, c := range collectors {
		g := &Gauges{descs: map[string]Desc{}, values: map[string]map[string]float64{}}
		names := make([]string, 0, len(c.families))
		for _, d := range c.families {
			g.descs[d.Name] = d
			names = append(names, d.Name)
		}
		if err := c.read(ctx, g); err != nil {
			if r.Trouble != nil {
				r.Trouble(names, err)
			}
			continue
		}
		for _, d := range c.families {
			header(w, d.Name, d.Help, "gauge")
			values := g.values[d.Name]
			for _, k := range sortedKeys(values) {
				sample(w, d.Name, d.Labels, unkey(k), "", "", values[k])
			}
		}
	}
	return w.Flush()
}

func (c *Counter) write(w *bufio.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	header(w, c.name, c.help, "counter")
	for _, k := range sortedKeys(c.series) {
		sample(w, c.name, c.labels, unkey(k), "", "", c.series[k])
	}
}

func (h *Histogram) write(w *bufio.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	header(w, h.name, h.help, "histogram")
	for _, k := range sortedKeys(h.series) {
		d, values := h.series[k], unkey(k)
		var cumulative uint64
		for i, bound := range h.buckets {
			cumulative += d.counts[i]
			sample(w, h.name+"_bucket", h.labels, values, "le", number(bound), float64(cumulative))
		}
		sample(w, h.name+"_bucket", h.labels, values, "le", "+Inf", float64(d.count))
		sample(w, h.name+"_sum", h.labels, values, "", "", d.sum)
		sample(w, h.name+"_count", h.labels, values, "", "", float64(d.count))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// header is the HELP and TYPE lines of a family.
func header(w *bufio.Writer, name, help, kind string) {
	help = strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(help)
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// sample is one line: the name, the labels with the extra one a bucket carries, and the value.
func sample(w *bufio.Writer, name string, labels, values []string, extra, extraValue string, v float64) {
	w.WriteString(name)
	if len(labels) > 0 || extra != "" {
		w.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				w.WriteByte(',')
			}
			fmt.Fprintf(w, "%s=\"%s\"", l, escape(values[i]))
		}
		if extra != "" {
			if len(labels) > 0 {
				w.WriteByte(',')
			}
			fmt.Fprintf(w, "%s=\"%s\"", extra, extraValue)
		}
		w.WriteByte('}')
	}
	w.WriteByte(' ')
	w.WriteString(number(v))
	w.WriteByte('\n')
}

// escape is a label value as the format writes one: a backslash, a double quote and a line feed
// escaped, and everything else as it is.
func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// number is a value as the format writes one, with the infinities and NaN spelled its way.
func number(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
