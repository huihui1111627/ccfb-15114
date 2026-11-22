package sortengine

import (
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Options tune one Sort call.
type Options struct {
	// TopK, when > 0, keeps only the first TopK rows instead of ordering all
	// of them. The result is identical to slicing a full sort.
	TopK int
	// Samples is the number of leading input rows whose encoded keys are
	// copied into the result for display. Zero disables sampling.
	Samples int
	// Parallel forces the first-byte bucket path on or off. When nil the
	// Spec's Parallel flag and MinParallelRows decide.
	Parallel *bool
	// Workers overrides the worker count (default runtime.GOMAXPROCS(0)).
	Workers int
	// NoSort skips sorting and returns only encoding statistics.
	NoSort bool
}

// Stats reports what the engine did, for the demo page and for the
// verification program's timing table.
type Stats struct {
	Rows      int     `json:"rows"`
	KeyBytes  int     `json:"keyBytes"`
	AvgKeyLen float64 `json:"avgKeyLen"`
	Columns   int     `json:"columns"`

	EncodeNs     int64 `json:"encodeNs"`
	PartitionNs  int64 `json:"partitionNs"`
	BucketSortNs int64 `json:"bucketSortNs"`
	HeapNs       int64 `json:"heapNs"`
	ConcatNs     int64 `json:"concatNs"`
	TotalNs      int64 `json:"totalNs"`

	Mode          string       `json:"mode"`
	EncodeWorkers int          `json:"encodeWorkers"`
	Parallel      bool         `json:"parallel"`
	Workers       int          `json:"workers"`
	Buckets       int          `json:"buckets"`
	MaxBucketRows int          `json:"maxBucketRows"`
	UsedInsertion bool         `json:"usedInsertion"`
	InsertionRuns int          `json:"insertionRuns"`
	RadixPasses   int          `json:"radixPasses"`
	HeapSwaps     int          `json:"heapSwaps"`
	TopK          int          `json:"topK"`
	BucketsList   []BucketInfo `json:"bucketsList,omitempty"`
	WorkerSpans   []WorkerSpan `json:"workerSpans,omitempty"`
}

// BucketInfo describes one first-byte bucket in the parallel path.
type BucketInfo struct {
	Rows      int    `json:"rows"`
	FirstByte string `json:"firstByte"`
	SortedBy  int    `json:"sortedBy"` // worker that sorted it
}

// WorkerSpan is one worker's share of the parallel bucket sort.
type WorkerSpan struct {
	Worker  int   `json:"worker"`
	Buckets int   `json:"buckets"`
	Rows    int   `json:"rows"`
	StartNs int64 `json:"startNs"`
	DurNs   int64 `json:"durNs"`
}

// ColumnSample shows how one column's bytes look inside a sample key.
type ColumnSample struct {
	Name  string `json:"name"`
	Bytes string `json:"bytes"`
	Note  string `json:"note"`
}

// KeySample is one encoded key broken down per column.
type KeySample struct {
	Row     int            `json:"row"`
	Key     string         `json:"key"`
	Length  int            `json:"length"`
	Columns []ColumnSample `json:"columns"`
}

// Result is the outcome of a sort.
type Result struct {
	Order   []int32     `json:"order"`
	Stats   Stats       `json:"stats"`
	Samples []KeySample `json:"samples,omitempty"`
}

// SortedRows materialises the sorted input rows.
func (r *Result) SortedRows(rows []Row) []Row {
	out := make([]Row, len(r.Order))
	for i, idx := range r.Order {
		out[i] = rows[idx]
	}
	return out
}

// keyStore packs every encoded key into one arena; off[i]..off[i+1] is row i's
// key. One allocation for all keys keeps the encode loop tight and the radix
// passes cache friendly.
type keyStore struct {
	arena []byte
	off   []int64 // len(rows)+1
}

func (k *keyStore) key(i int32) []byte { return k.arena[k.off[i]:k.off[i+1]] }

func (k *keyStore) byteAt(i int32, depth int) byte {
	start, end := k.off[i], k.off[i+1]
	if int64(depth) >= end-start {
		return 0
	}
	return k.arena[start+int64(depth)]
}

func (k *keyStore) compare(a, b int32) int {
	return compareEncoded(k.arena[k.off[a]:k.off[a+1]], k.arena[k.off[b]:k.off[b+1]])
}

// Sort orders rows according to the compiled spec. It never mutates rows or
// the spec, so one Sorter is safe to share across goroutines after New.
func (s *Sorter) Sort(rows []Row, opts Options) *Result {
	startAll := time.Now()
	n := len(rows)
	res := &Result{}
	st := &res.Stats
	st.Rows = n
	st.Columns = len(s.cols)
	st.TopK = opts.TopK

	if n == 0 {
		st.Mode = "empty"
		st.TotalNs = time.Since(startAll).Nanoseconds()
		return res
	}

	// ---- phase 1: evaluate expressions and encode every key ----
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	encStart := time.Now()
	ks, encodeWorkers := s.encodeAll(rows, workers)
	st.EncodeNs = time.Since(encStart).Nanoseconds()
	st.KeyBytes = len(ks.arena)
	st.AvgKeyLen = float64(len(ks.arena)) / float64(n)
	st.EncodeWorkers = encodeWorkers
	if opts.Samples > 0 {
		res.Samples = s.samples(rows, &ks, opts.Samples)
	}
	if opts.NoSort {
		st.Mode = "encode-only"
		st.TotalNs = time.Since(startAll).Nanoseconds()
		return res
	}

	parallel := s.spec.Parallel
	minRows := s.spec.MinParallelRows
	if minRows <= 0 {
		minRows = DefaultMinParallelRows
	}
	if opts.Parallel != nil {
		parallel = *opts.Parallel
	}
	parallel = parallel && n >= minRows && workers > 1

	if opts.TopK > 0 {
		s.topK(&ks, n, opts.TopK, res, startAll)
		return res
	}

	idx := make([]int32, n)
	for i := range idx {
		idx[i] = int32(i)
	}

	if parallel {
		s.sortParallel(&ks, idx, workers, res, startAll)
	} else {
		s.sortSerial(&ks, idx, res, startAll)
	}
	res.Order = idx
	return res
}

func (s *Sorter) estimatedKeyLen() int {
	total := 0
	for _, c := range s.cols {
		if w := KeyWidth(c.col.Kind); w > 0 {
			total += w
		} else {
			total += 24
		}
	}
	return total
}

// encodeAll evaluates the key expressions and packs every key into one arena.
// For large inputs the row range is split across workers, each writing a
// contiguous arena segment; the segments are then joined with one copy.
// The encoded layout does not depend on the worker count.
func (s *Sorter) encodeAll(rows []Row, workers int) (keyStore, int) {
	n := len(rows)
	ks := keyStore{off: make([]int64, n+1)}
	if workers <= 1 || n < 4096 {
		buf := encBuf{offsets: make([]int, len(s.cols)+1)}
		ks.arena = make([]byte, 0, s.capHint(n))
		for i := range rows {
			key := s.appendKeyWithOffsets(&buf, rows[i])
			ks.arena = append(ks.arena, key...)
			ks.off[i+1] = int64(len(ks.arena))
		}
		return ks, 1
	}
	// Even chunks keep per-worker work balanced; the final chunk may be short.
	chunk := (n + workers - 1) / workers
	type part struct {
		lo, hi  int
		arena   []byte
		offsets []int64
	}
	parts := make([]part, 0, workers)
	for lo := 0; lo < n; lo += chunk {
		hi := lo + chunk
		if hi > n {
			hi = n
		}
		parts = append(parts, part{lo: lo, hi: hi})
	}
	var wg sync.WaitGroup
	for w := range parts {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			p := &parts[w]
			buf := encBuf{offsets: make([]int, len(s.cols)+1)}
			p.arena = make([]byte, 0, s.capHint(p.hi-p.lo))
			p.offsets = make([]int64, p.hi-p.lo+1)
			for i := p.lo; i < p.hi; i++ {
				key := s.appendKeyWithOffsets(&buf, rows[i])
				p.arena = append(p.arena, key...)
				p.offsets[i-p.lo+1] = int64(len(p.arena))
			}
		}(w)
	}
	wg.Wait()
	total := 0
	for i := range parts {
		total += len(parts[i].arena)
	}
	ks.arena = make([]byte, 0, total)
	var base int64
	rowBase := 0
	for i := range parts {
		p := &parts[i]
		ks.arena = append(ks.arena, p.arena...)
		for j := 1; j < len(p.offsets); j++ {
			ks.off[rowBase+j] = base + p.offsets[j]
		}
		base += int64(len(p.arena))
		rowBase += p.hi - p.lo
		p.arena = nil
	}
	return ks, len(parts)
}

func (s *Sorter) capHint(n int) int {
	c := s.estimatedKeyLen() * n
	if c < 0 || c > 1<<31 {
		c = 1 << 31
	}
	return c
}

// samples re-evaluates the first n rows to render their keys per column.
func (s *Sorter) samples(rows []Row, ks *keyStore, n int) []KeySample {
	if n > len(rows) {
		n = len(rows)
	}
	out := make([]KeySample, 0, n)
	buf := encBuf{offsets: make([]int, len(s.cols)+1)}
	for i := 0; i < n; i++ {
		key := s.appendKeyWithOffsets(&buf, rows[i])
		smp := KeySample{Row: i, Key: KeyHex(key), Length: len(key)}
		smp.Columns = make([]ColumnSample, len(s.cols))
		for c := range s.cols {
			lo, hi := buf.offsets[c], buf.offsets[c+1]
			smp.Columns[c] = ColumnSample{
				Name:  s.cols[c].col.Name,
				Bytes: KeyHex(key[lo:hi]),
				Note:  s.cols[c].expr.String() + " = " + Coerce(s.cols[c].expr.Eval(rows[i]), s.cols[c].col.Kind).String(),
			}
		}
		out = append(out, smp)
	}
	return out
}

// sortSerial runs one top-level MSD radix sort over all rows.
func (s *Sorter) sortSerial(ks *keyStore, idx []int32, res *Result, startAll time.Time) {
	st := &res.Stats
	st.Mode = "radix-msd"
	st.Parallel = false
	st.Workers = 1
	var m counters
	sortStart := time.Now()
	scratch := make([]int32, len(idx))
	radixRange(ks, idx, scratch, 0, &m)
	st.BucketSortNs = time.Since(sortStart).Nanoseconds()
	st.UsedInsertion = m.insertion > 0
	st.InsertionRuns = m.insertion
	st.RadixPasses = m.passes
	st.MaxBucketRows = m.maxBucket
	st.Buckets = 1
	st.TotalNs = time.Since(startAll).Nanoseconds()
}

type counters struct {
	insertion int
	passes    int
	maxBucket int
}

const radixInsertionThreshold = 24

// radixRange sorts idx in place, treating bytes before depth as equal. idx and
// scratch have the same length; scratch is used by the stable counting-sort
// pass and each recursive bucket gets the matching scratch sub-slice.
//
// Every key carries every column, and every column is self-delimiting, so no
// encoded key is a proper prefix of another: two distinct keys always differ at
// some byte both contain. That is what lets the leaf tie-break (input order)
// stand for full-key equality.
func radixRange(ks *keyStore, idx, scratch []int32, depth int, m *counters) {
	for len(idx) > 1 {
		if len(idx) <= radixInsertionThreshold {
			insertionSort(ks, idx)
			m.insertion++
			return
		}
		var minB, maxB byte = 0xff, 0x00
		any := false
		for _, i := range idx {
			start, end := ks.off[i], ks.off[i+1]
			if int64(depth) < end-start {
				any = true
				b := ks.arena[start+int64(depth)]
				if b < minB {
					minB = b
				}
				if b > maxB {
					maxB = b
				}
			}
		}
		if !any {
			return
		}
		if minB == maxB {
			depth++
			continue
		}
		m.passes++
		if len(idx) > m.maxBucket {
			m.maxBucket = len(idx)
		}
		// Stable counting sort by the byte at depth.
		var count [257]int32
		for _, i := range idx {
			count[int(ks.byteAt(i, depth))+1]++
		}
		for b := 1; b < len(count); b++ {
			count[b] += count[b-1]
		}
		// count[b] is the start of bucket b and count[b+1] its end. Walk the
		// input in order so buckets are filled stably.
		for _, i := range idx {
			b := ks.byteAt(i, depth)
			scratch[count[b]] = i
			count[b]++
		}
		copy(idx, scratch)
		// After the fill loop count[b] is one past bucket b's end and count is
		// non-decreasing, so bucket b spans (prev, count[b]] where prev is the
		// previous bucket's end. That is exactly the loop below.
		start := int32(0)
		for b := 0; b < 256; b++ {
			end := count[b]
			if end-start > 1 {
				radixRange(ks, idx[start:end], scratch[start:end], depth+1, m)
			}
			start = end
		}
		return
	}
}

// insertionSort is stable: it only moves an element past a strictly greater
// key, so equal keys keep their relative order.
func insertionSort(ks *keyStore, idx []int32) {
	for i := 1; i < len(idx); i++ {
		v := idx[i]
		j := i - 1
		for j >= 0 && ks.compare(v, idx[j]) < 0 {
			idx[j+1] = idx[j]
			j--
		}
		idx[j+1] = v
	}
}

type leafRange struct {
	start, end int
	depth      int
}

// sortParallel splits the rows into first-byte buckets (recursing into more
// bytes only when a bucket is still too large), sorts each leaf independently
// on a worker, and leaves them adjacent in idx so concatenating by bucket
// number is the identity. No merge step is needed because every byte of every
// key in leaf k is not necessarily smaller than leaf k+1's — but the partition
// was produced by stable counting sorts on leading byte ranges, so the leaves
// are already in lexicographic relation and their concatenation is sorted.
func (s *Sorter) sortParallel(ks *keyStore, idx []int32, workers int, res *Result, startAll time.Time) {
	n := len(idx)
	st := &res.Stats
	st.Mode = "parallel-first-byte"
	st.Parallel = true
	st.Workers = workers

	// Aim for a few leaves per worker so the run has no long idle tail, but do
	// not shred small inputs into tiny leaves.
	chunk := n / (workers * 4)
	if chunk < 4096 {
		chunk = 4096
	}

	partStart := time.Now()
	var leaves []leafRange
	scratch := make([]int32, n)
	partitionLeaves(ks, idx, scratch, 0, n, 0, chunk, &leaves)
	st.PartitionNs = time.Since(partStart).Nanoseconds()
	st.Buckets = len(leaves)

	maxLeaf := 0
	for _, lf := range leaves {
		if lf.end-lf.start > maxLeaf {
			maxLeaf = lf.end - lf.start
		}
	}
	st.MaxBucketRows = maxLeaf

	k := workers
	if k > len(leaves) {
		k = len(leaves)
	}
	st.WorkerSpans = make([]WorkerSpan, k)
	st.BucketsList = make([]BucketInfo, 0, len(leaves))
	bucketOwner := make([]int, len(leaves))

	var next atomic.Int64
	sortStart := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	totalInsertion, totalPasses := 0, 0
	for w := 0; w < k; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make([]int32, maxLeaf)
			span := WorkerSpan{Worker: w}
			t0 := time.Since(startAll).Nanoseconds()
			li, lp := 0, 0
			for {
				j := int(next.Add(1)) - 1
				if j >= len(leaves) {
					break
				}
				lf := leaves[j]
				bucketOwner[j] = w
				rows := lf.end - lf.start
				span.Buckets++
				span.Rows += rows
				if rows > 1 {
					var m counters
					radixRange(ks, idx[lf.start:lf.end], local[:rows], lf.depth, &m)
					li += m.insertion
					lp += m.passes
				}
			}
			span.StartNs = t0
			span.DurNs = time.Since(startAll).Nanoseconds() - t0
			st.WorkerSpans[w] = span
			mu.Lock()
			totalInsertion += li
			totalPasses += lp
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	st.BucketSortNs = time.Since(sortStart).Nanoseconds()
	st.InsertionRuns = totalInsertion
	st.UsedInsertion = totalInsertion > 0
	st.RadixPasses = totalPasses
	st.ConcatNs = 0 // leaves are already adjacent: concatenation is the identity
	for j, lf := range leaves {
		st.BucketsList = append(st.BucketsList, BucketInfo{
			Rows:      lf.end - lf.start,
			FirstByte: fmt.Sprintf("%02x", ks.byteAt(idx[lf.start], 0)),
			SortedBy:  bucketOwner[j],
		})
	}
	st.TotalNs = time.Since(startAll).Nanoseconds()
}

// partitionLeaves recursively splits idx[start:end] by the byte at depth until
// each piece is small enough, and records pieces in byte order. The split is a
// stable counting sort, so the pieces end up contiguous in idx in key order.
//
// Every level re-buckets only the pieces that are still too large and skips
// bytes all of a piece's keys share, so the cost is proportional to the number
// of keys that actually need splitting, not to 256 bytes per level.
func partitionLeaves(ks *keyStore, idx, scratch []int32, lo, hi, depth, chunk int, out *[]leafRange) {
	if hi-lo <= chunk || hi-lo <= 1 {
		*out = append(*out, leafRange{lo, hi, depth})
		return
	}
	// If every key in the range shares the byte at depth, skip it without
	// bucketing. This is the common case inside a busy first-byte bucket whose
	// rows all share the zone prefix: everything is equal for several bytes.
	for {
		var first byte
		shared := true
		for _, i := range idx[lo:hi] {
			start, end := ks.off[i], ks.off[i+1]
			var b byte
			if int64(depth) < end-start {
				b = ks.arena[start+int64(depth)]
			}
			if b != first {
				shared = false
				break
			}
		}
		if !shared || hi-lo <= 1 {
			break
		}
		depth++
	}
	// Count this byte's distribution.
	var count [256]int32
	for _, i := range idx[lo:hi] {
		count[ks.byteAt(i, depth)]++
	}
	nonEmpty := 0
	maxBucket := int32(0)
	for _, c := range count {
		if c > 0 {
			nonEmpty++
			if c > maxBucket {
				maxBucket = c
			}
		}
	}
	if nonEmpty <= 1 {
		// Only one byte value left but the range is still too large: widen
		// with recursion on the next depth (the piece is already contiguous).
		partitionLeaves(ks, idx, scratch, lo, hi, depth+1, chunk, out)
		return
	}
	// Stable counting sort of this range using the caller-provided scratch.
	var start [256]int32
	var acc int32
	for b := 0; b < 256; b++ {
		start[b] = acc
		acc += count[b]
	}
	for _, i := range idx[lo:hi] {
		b := ks.byteAt(i, depth)
		scratch[start[b]] = i
		start[b]++
	}
	copy(idx[lo:hi], scratch[:hi-lo])
	// Children are contiguous slices of idx starting at lo, in byte order.
	// Process them in place; each child's own scratch is reused from the top.
	var offsets [257]int32
	offsets[0] = 0
	for b := 0; b < 256; b++ {
		offsets[b+1] = offsets[b] + count[b]
	}
	for b := 0; b < 256; b++ {
		cs := lo + int(offsets[b])
		ce := lo + int(offsets[b+1])
		if ce > cs {
			partitionLeaves(ks, idx, scratch, cs, ce, depth+1, chunk, out)
		}
	}
}

// topK keeps the best k rows in a max-heap ordered by the encoded key, then
// sorts the survivors so the result equals the first k rows of a full sort.
func (s *Sorter) topK(ks *keyStore, n, k int, res *Result, startAll time.Time) {
	st := &res.Stats
	st.Mode = "heap-topk"
	st.Parallel = false
	st.Workers = 1
	if k > n {
		k = n
	}
	heapStart := time.Now()
	h := &indexHeap{ks: ks, a: make([]int32, 0, k)}
	for i := 0; i < n; i++ {
		x := int32(i)
		if len(h.a) < k {
			h.push(x)
			continue
		}
		if h.less(x, h.a[0]) {
			h.a[0] = x
			h.siftDown(0)
			st.HeapSwaps++
		}
	}
	st.HeapNs = time.Since(heapStart).Nanoseconds()

	// The survivors leave the heap in eviction order, not input order, while
	// the radix sort's leaf tie-break is input order. Restoring index order
	// first makes that tie-break correct again.
	sort.Slice(h.a, func(i, j int) bool { return h.a[i] < h.a[j] })
	sortStart := time.Now()
	radixRange(ks, h.a, make([]int32, len(h.a)), 0, &counters{})
	st.BucketSortNs = time.Since(sortStart).Nanoseconds()
	st.Buckets = 1
	st.MaxBucketRows = len(h.a)
	st.UsedInsertion = len(h.a) > 1
	st.TotalNs = time.Since(startAll).Nanoseconds()
	res.Order = h.a
}

// indexHeap is a max-heap on (encoded key, then input index). The index
// tie-break makes "smaller" mean "would come first in a stable full sort",
// which is exactly the heap's eviction order.
type indexHeap struct {
	ks *keyStore
	a  []int32
}

func (h *indexHeap) less(i, j int32) bool {
	if c := h.ks.compare(i, j); c != 0 {
		return c < 0
	}
	return i < j
}

func (h *indexHeap) push(x int32) {
	h.a = append(h.a, x)
	i := len(h.a) - 1
	for i > 0 {
		p := (i - 1) / 2
		if !h.less(h.a[p], h.a[i]) {
			break
		}
		h.a[p], h.a[i] = h.a[i], h.a[p]
		i = p
	}
}

func (h *indexHeap) siftDown(i int) {
	n := len(h.a)
	for {
		l := 2*i + 1
		if l >= n {
			return
		}
		big := l
		if r := l + 1; r < n && h.less(h.a[l], h.a[r]) {
			big = r
		}
		if !h.less(h.a[i], h.a[big]) {
			return
		}
		h.a[i], h.a[big] = h.a[big], h.a[i]
		i = big
	}
}
