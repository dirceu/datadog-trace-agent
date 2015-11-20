package quantile

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"math/rand"
)

/*
FIXME: shamelessly copied from dgryski/go-gk, not verified, not really tested
Should reimplement everything from scratch from the paper

"Space-Efficient Online Computation of Quantile Summaries" (Greenwald, Khanna 2001)

http://infolab.stanford.edu/~datar/courses/cs361a/papers/quantiles.pdf

This implementation is backed by a skiplist to make inserting elements into the
summary faster.  Querying is still O(n).

*/

// EPSILON is the precision of the rank returned by our quantile queries
const EPSILON float64 = 0.01

// Summary is a way to represent an approximation of the distribution of values
type Summary struct {
	data        *Skiplist // where the real data is stored
	EncodedData []Entry   `json:"data"` // flattened data user for ser/deser purposes
	N           int       `json:"n"`    // number of unique points that have been added to this summary
}

// Entry is an element of the skiplist, see GK paper for description
type Entry struct {
	V       int64    `json:"v"`
	G       int      `json:"g"`
	Delta   int      `json:"delta"`
	Samples []uint64 `json:"samples"` // Span IDs of traces representing this part of the spectrum
}

// NewSummary returns a new approx-summary with accuracy EPSILON
func NewSummary() *Summary {
	return &Summary{
		data: NewSkiplist(),
	}
}

// MarshalJSON is used to send the data over to the API
func (s Summary) MarshalJSON() ([]byte, error) {
	if s.data == nil {
		panic(errors.New("Cannot marshal non-initialized Summary"))
	}

	// TODO[leo] preallocate, not sure: 1/ 2*EPSILON?
	s.EncodedData = make([]Entry, 0)
	curr := s.data.head
	for curr != nil {
		s.EncodedData = append(s.EncodedData, curr.value)
		curr = curr.next[0]
	}

	return json.Marshal(map[string]interface{}{
		"data": s.EncodedData,
		"n":    s.N,
	})
}

// Avoid infinite recursion when unmarshalling
// When unmarshalling bytes in a Summary{} struct, it will call UnmarshalJSON recursively because Summary{} implements the unmarshaller interface
// using the private type summary here, tricks the unmarshaller into running the regular JSON unmarshalling.
type summary Summary

// UnmarshalJSON is used to recreate a Summary structure from a JSON payload
// It reinserts points artificially (TODO: see if this is OK?)
func (s *Summary) UnmarshalJSON(b []byte) error {
	ss := summary{}
	err := json.Unmarshal(b, &ss)
	if err != nil {
		return err
	}
	*s = Summary(ss)

	s.data = NewSkiplist()
	for _, e := range s.EncodedData {
		s.data.Insert(e)
	}

	return nil
}

// GobEncode is used by the Kafka payload now, it flattens our skiplist
func (s *Summary) GobEncode() ([]byte, error) {
	// TODO[leo] preallocate, not sure: 1/ 2*EPSILON?
	s.EncodedData = make([]Entry, 0)
	curr := s.data.head
	for curr != nil {
		s.EncodedData = append(s.EncodedData, curr.value)
		curr = curr.next[0]
	}
	ss := summary(*s)

	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	err := encoder.Encode(ss)
	return buf.Bytes(), err
}

// GobDecode recreates a skiplist, TODO[leo] is the skiplist recreated as is?
func (s *Summary) GobDecode(data []byte) error {
	ss := summary{}
	buf := bytes.NewBuffer(data)
	decoder := gob.NewDecoder(buf)
	if err := decoder.Decode(&ss); err != nil {
		return err
	}

	*s = Summary(ss)
	s.data = NewSkiplist()
	for _, e := range s.EncodedData {
		s.data.Insert(e)
	}

	return nil
}

// Insert inserts a new value v in the summary paired with t (the ID of the span it was reported from)
func (s *Summary) Insert(v int64, t uint64) {
	e := Entry{
		V:       v,
		G:       1,
		Delta:   0,
		Samples: []uint64{t},
	}

	eptr := s.data.Insert(e)

	s.N++

	if eptr.prev[0] != s.data.head && eptr.next[0] != nil {
		eptr.value.Delta = int(2 * EPSILON * float64(s.N))
	}

	if s.N%int(1.0/float64(2.0*EPSILON)) == 0 {
		s.compress()
	}
}

func (s *Summary) compress() {
	var missing int

	epsN := int(2 * EPSILON * float64(s.N))

	for elt := s.data.head.next[0]; elt != nil && elt.next[0] != nil; {
		next := elt.next[0]
		t := elt.value
		nt := &next.value

		// value merging
		if t.V == nt.V {
			missing += nt.G
			nt.Delta += missing
			nt.G = t.G
			nt.Samples = append(nt.Samples, t.Samples...)
			s.data.Remove(elt)
		} else if t.G+nt.G+missing+nt.Delta < epsN {
			nt.G += t.G + missing
			nt.Samples = append(nt.Samples, t.Samples...)
			missing = 0
			s.data.Remove(elt)
		} else {
			nt.G += missing
			missing = 0
		}
		elt = next
	}
}

// Quantile returns an EPSILON estimate of the element at quantile 'q' (0 <= q <= 1)
func (s *Summary) Quantile(q float64) (int64, []uint64) {

	// convert quantile to rank
	r := int(q*float64(s.N) + 0.5)

	var rmin int
	epsN := int(EPSILON * float64(s.N))

	for elt := s.data.head.next[0]; elt != nil; elt = elt.next[0] {
		t := elt.value
		rmin += t.G
		n := elt.next[0]

		if n == nil {
			return t.V, t.Samples
		}

		if r+epsN < rmin+n.value.G+n.value.Delta {
			if r+epsN < rmin+n.value.G {
				return t.V, t.Samples
			}
			return n.value.V, n.value.Samples
		}
	}

	panic("not reached")
}

// SummarySlice reprensents how many values are in a [Start, End] range
type SummarySlice struct {
	Start   int64
	End     int64
	Weight  int
	Samples []uint64
}

// BySlices returns a slice of Summary slices that represents weighted ranges of
// values
// e.g.    [0, 1]  : 3
//		   [1, 23] : 12 ...
// The number of intervals is related to the precision kept in the internal
// data structure to ensure epsilon*s.N precision on quantiles, but it's bounded.
// The weights are not exact, they're only upper bounds (see GK paper).
func (s *Summary) BySlices() []SummarySlice {
	var slices []SummarySlice

	last := s.data.head
	cur := last.next[0]

	for cur != nil {
		ss := SummarySlice{
			Start:   last.value.V,
			End:     cur.value.V,
			Weight:  cur.value.G + cur.value.Delta - 1, // see GK paper section 2.1
			Samples: cur.value.Samples,
		}
		slices = append(slices, ss)

		last = cur
		cur = cur.next[0]
	}

	return slices
}

// Merge takes a summary and merge the values inside the current pointed object
func (s *Summary) Merge(s2 *Summary) {
	if s2.N == 0 || s2.data == nil {
		return
	}

	s.N += s2.N
	// Iterate on s2 elements and insert/merge them
	curElt := s2.data.head
	for curElt != nil {
		s.data.Insert(curElt.value)
		curElt = curElt.next[0]
	}
	// Force compression
	s.compress()
}

const maxHeight = 31

// Skiplist is a pseudo-random data structure used to store nodes and find quickly what we want
type Skiplist struct {
	height int
	head   *SkiplistNode
}

// SkiplistNode is holding the actual value and pointers to the neighbor nodes
type SkiplistNode struct {
	value Entry
	next  []*SkiplistNode
	prev  []*SkiplistNode
}

// NewSkiplist returns a new empty Skiplist
func NewSkiplist() *Skiplist {
	return &Skiplist{
		height: 0,
		head:   &SkiplistNode{next: make([]*SkiplistNode, maxHeight)},
	}
}

// Insert adds a new Entry to the Skiplist and yields a pointer to the node where the data was inserted
func (s *Skiplist) Insert(e Entry) *SkiplistNode {
	level := 0

	n := rand.Int31()
	for n&1 == 1 {
		level++
		n >>= 1
	}

	if level > s.height {
		s.height++
		level = s.height
	}

	node := &SkiplistNode{
		value: e,
		next:  make([]*SkiplistNode, level+1),
		prev:  make([]*SkiplistNode, level+1),
	}
	curr := s.head
	for i := s.height; i >= 0; i-- {

		for curr.next[i] != nil && e.V >= curr.next[i].value.V {
			curr = curr.next[i]
		}

		if i > level {
			continue
		}

		node.next[i] = curr.next[i]
		if curr.next[i] != nil && curr.next[i].prev[i] != nil {
			curr.next[i].prev[i] = node
		}
		curr.next[i] = node
		node.prev[i] = curr
	}

	return node
}

// Remove removes a node from the Skiplist
func (s *Skiplist) Remove(node *SkiplistNode) {

	// remove n from each level of the Skiplist

	for i := range node.next {
		prev := node.prev[i]
		next := node.next[i]

		if prev != nil {
			prev.next[i] = next
		}
		if next != nil {
			next.prev[i] = prev
		}
		node.next[i] = nil
		node.prev[i] = nil
	}
}