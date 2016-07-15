package sampler

import (
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	log "github.com/cihub/seelog"

	"github.com/DataDog/raclette/config"
	"github.com/DataDog/raclette/model"
)

// Signature is a simple representation of trace, used to identify simlar traces
type Signature uint64

// SignatureSampler samples by identifying traces with a signature then score it
type SignatureSampler struct {
	// Last time we sampled a given signature (epoch in seconds)
	lastTSBySignature map[Signature]float64
	// Traces sampled kept until the next flush
	sampledTraces []model.Trace

	// Scoring configuration
	sMin   float64 // Score required to be sampled, sample when score is over sMin
	theta  float64 // Typical last-seen duration (in s) after which we want to sample a trace
	jitter float64 // Multiplicative random coefficient (0 to 1)

	mu sync.Mutex
}

// NewSignatureSampler creates a new SignatureSampler, ready to ingest traces
func NewSignatureSampler(conf *config.AgentConfig) *SignatureSampler {
	// TODO: have a go-routine expiring old signatures from lastTSBySignature

	return &SignatureSampler{
		lastTSBySignature: map[Signature]float64{},
		sampledTraces:     []model.Trace{},

		// Sane defaults
		sMin:   conf.SamplerSMin,
		theta:  conf.SamplerTheta,
		jitter: conf.SamplerJitter,
	}
}

// AddTrace samples a trace then keep it until the next flush
func (s *SignatureSampler) AddTrace(trace model.Trace) {
	signature := s.ComputeSignature(trace)

	s.mu.Lock()

	score := s.GetScore(signature)
	if score > s.sMin {
		s.sampledTraces = append(s.sampledTraces, trace)
		s.lastTSBySignature[signature] = float64(time.Now().UnixNano()) / 1e9
	}

	s.mu.Unlock()

	log.Debugf("trace_id:%v signature:%v score:%v sampled:%v", trace[0].TraceID, signature, score, (score > s.sMin))
}

// GetScore gives a score to a trace reflecting how strong we want to sample it
// Current implementation only cares about the last time a similar trace was seen + some randomness
func (s *SignatureSampler) GetScore(signature Signature) float64 {
	timeScore := s.GetTimeScore(signature)

	// Add some jitter
	return timeScore * (1 + s.jitter*(1-2*rand.Float64()))
}

// GetTimeScore gives a score based on the square root of the last time this signature was seen.
// Score from 0 to 5 (will matter when we will combine different scores together)
func (s *SignatureSampler) GetTimeScore(signature Signature) float64 {
	// Last time seen score: from 0 to 5.
	ts, seen := s.lastTSBySignature[signature]
	if !seen {
		return 5
	}
	delta := float64(time.Now().UnixNano())/1e9 - ts

	if delta <= 0 {
		return 0
	}

	return math.Min(math.Sqrt(delta/s.theta), 5)
}

// spanHash is the type of the hashes used during the computation of a signature
// Use FNV for hashing since it is super-cheap and we have no cryptographic needs
type spanHash uint32
type spanHashSlice []spanHash

func (p spanHashSlice) Len() int           { return len(p) }
func (p spanHashSlice) Less(i, j int) bool { return p[i] < p[j] }
func (p spanHashSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func SortHashes(hashes []spanHash)         { sort.Sort(spanHashSlice(hashes)) }

// ComputeSignature generates a signature of a trace
// Signature based on the hash of (service, name, resource, is_error) for the root, plus the set of
// (service, name, is_error) of each span.
func (s *SignatureSampler) ComputeSignature(trace model.Trace) Signature {
	traceHash := computeRootHash(s.getRoot(trace))
	spanHashes := make([]spanHash, len(trace))

	for i := range trace {
		spanHashes = append(spanHashes, computeSpanHash(trace[i]))
	}

	// Now sort, dedupe then merge all the hashes to build the signature
	SortHashes(spanHashes)

	last := spanHashes[0]
	idx := 1
	for i := 1; i < len(spanHashes); i++ {
		if spanHashes[i] != last {
			last = spanHashes[i]
			spanHashes[idx] = last
			idx++
		}
	}
	// spanHashes[:idx] is the sorted and deduped slice

	// Build the signature like a barbarian (with a XOR of all the hashes).
	// Stupid but cheap and does the job for now.
	for i := 0; i < idx; i++ {
		traceHash = spanHashes[i] ^ traceHash
	}

	return Signature(traceHash)
}

func computeSpanHash(span model.Span) spanHash {
	h := fnv.New32a()
	h.Write([]byte(span.Service))
	h.Write([]byte(span.Name))
	h.Write([]byte{byte(span.Error)})

	return spanHash(h.Sum32())
}

func computeRootHash(span model.Span) spanHash {
	h := fnv.New32a()
	h.Write([]byte(span.Service))
	h.Write([]byte(span.Name))
	h.Write([]byte(span.Resource))
	h.Write([]byte{byte(span.Error)})

	return spanHash(h.Sum32())
}

// getRoot extract the root span from a trace
func (s *SignatureSampler) getRoot(trace model.Trace) model.Span {
	// This current implementation is not 100% reliable, and would be wrong if we receive a sub-trace with its local
	// root not being at the end
	for i := range trace {
		if trace[len(trace)-1-i].ParentID == 0 {
			return trace[len(trace)-1-i]
		}
	}
	return trace[len(trace)-1]
}

// Flush returns representative spans based on GetSamples and reset its internal memory
func (s *SignatureSampler) Flush() []model.Trace {
	s.mu.Lock()
	samples := s.sampledTraces
	s.sampledTraces = []model.Trace{}
	s.mu.Unlock()

	return samples
}
