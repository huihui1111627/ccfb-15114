// Command verify runs the conformance suite: for each generated sort spec it
// checks the engine's order against a column-by-column stable sort from the
// standard library, checks TopK against a prefix of the full order, and
// reports the timing ratio between the two approaches.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"time"

	"pigeonsort/internal/harness"
	"pigeonsort/sortengine"
)

func main() {
	var (
		cases   = flag.Int("cases", 400, "number of random spec/dataset combinations")
		rows    = flag.Int("rows", 3000, "rows per case")
		seed    = flag.Int64("seed", 1, "random seed")
		workers = flag.Int("workers", 0, "worker count (0 = GOMAXPROCS)")
		verbose = flag.Bool("v", false, "print every case")
		benchN  = flag.Int("bench-rows", 300000, "rows for the timing comparison")
		noBench = flag.Bool("no-bench", false, "skip the timing comparison")
	)
	flag.Parse()
	if *workers == 0 {
		*workers = runtime.GOMAXPROCS(0)
	}

	fmt.Printf("pigeon sort engine verification\n")
	fmt.Printf("Go %s, GOMAXPROCS=%d, seed=%d\n\n", runtime.Version(), *workers, *seed)

	rng := rand.New(rand.NewSource(*seed))
	fail := 0
	startAll := time.Now()
	for i := 0; i < *cases; i++ {
		c := harness.GenerateCase(rng, i, *rows)
		s, err := c.BuildSorter()
		if err != nil {
			fmt.Printf("case %3d BUILD ERROR: %v\n", i, err)
			fail++
			continue
		}
		res := s.Sort(c.Rows, sortengine.Options{Workers: *workers})
		want := s.ReferenceOrder(c.Rows)
		ok := equalOrder(res.Order, want)
		if !ok {
			fail++
			fmt.Printf("case %3d ORDER MISMATCH  %s\n", i, c.Title)
			reportMismatch(s, c.Rows, res.Order, want)
			continue
		}
		// TopK against the prefix of the full order.
		for _, k := range []int{1, 3, 25} {
			if k > len(c.Rows) {
				continue
			}
			top := s.Sort(c.Rows, sortengine.Options{TopK: k, Workers: *workers})
			if !equalOrder(top.Order, want[:k]) {
				fail++
				fmt.Printf("case %3d TOPK-%d MISMATCH  %s\n", i, k, c.Title)
				continue
			}
		}
		// Serial path (no first-byte partitioning) must agree with the
		// parallel one for the same input.
		serial := s.Sort(c.Rows, sortengine.Options{Parallel: boolPtr(false), Workers: *workers})
		if !equalOrder(serial.Order, want) {
			fail++
			fmt.Printf("case %3d SERIAL MISMATCH  %s\n", i, c.Title)
			continue
		}
		if *verbose {
			fmt.Printf("case %3d ok  rows=%d topk=%d keys=%dB avg=%.1f mode=%s  %s\n",
				i, len(c.Rows), c.TopK, res.Stats.KeyBytes, res.Stats.AvgKeyLen, res.Stats.Mode, c.Title)
		} else if (i+1)%50 == 0 {
			fmt.Printf("  %d/%d cases passed\n", i+1, *cases)
		}
	}
	fmt.Printf("\nconformance: %d cases, %d failed, %v\n", *cases, fail, time.Since(startAll).Round(time.Millisecond))

	if !*noBench {
		bench(*benchN, *workers)
	}
	if fail > 0 {
		os.Exit(1)
	}
	fmt.Println("\nALL CHECKS PASSED")
}

func boolPtr(b bool) *bool { return &b }

func equalOrder(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func reportMismatch(s *sortengine.Sorter, rows []sortengine.Row, got, want []int32) {
	shown := 0
	for i := range want {
		if got[i] == want[i] {
			continue
		}
		fmt.Printf("    pos %d: engine row %d, reference row %d\n", i, got[i], want[i])
		shown++
		if shown >= 3 {
			break
		}
	}
}

// bench compares the encoded-key engine with a standard-library stable sort
// that compares column by column, on a large race-season table.
func bench(n, workers int) {
	fmt.Printf("\ntiming comparison (%d rows)\n", n)
	rng := rand.New(rand.NewSource(99))
	rows := harness.GenerateRows(rng, n)
	schema := harness.Schema()
	cols := []sortengine.Column{}
	for _, e := range harness.ExprCatalog {
		if e.Name == "赛区" || e.Name == "分速=距离/用时" || e.Name == "归巢时刻" || e.Name == "鸽舍名" {
			col, err := sortengine.ParseColumn(e.Name, e.Kind.String(), e.Src, sortengine.Direction(len(cols)%2), sortengine.NullsFirst, e.Kind == sortengine.KindString)
			if err != nil {
				panic(err)
			}
			cols = append(cols, col)
		}
	}
	spec := sortengine.Spec{Columns: cols, Parallel: true, MinParallelRows: 20000}
	s, err := sortengine.New(spec, schema)
	if err != nil {
		panic(err)
	}
	// Warm up both paths.
	s.Sort(rows, sortengine.Options{Workers: workers})
	s.ReferenceOrder(rows)

	var engineBest, refBest time.Duration
	var stats sortengine.Stats
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		res := s.Sort(rows, sortengine.Options{Workers: workers})
		d := time.Since(t0)
		if engineBest == 0 || d < engineBest {
			engineBest, stats = d, res.Stats
		}
		t0 = time.Now()
		s.ReferenceOrder(rows)
		d = time.Since(t0)
		if refBest == 0 || d < refBest {
			refBest = d
		}
	}
	fmt.Printf("  engine (encode + radix) : %10v   (encode %v, partition %v, bucket sort %v)\n",
		engineBest.Round(time.Millisecond), time.Duration(stats.EncodeNs).Round(time.Millisecond),
		time.Duration(stats.PartitionNs).Round(time.Millisecond), time.Duration(stats.BucketSortNs).Round(time.Millisecond))
	fmt.Printf("  sort.SliceStable 逐列比较: %10v\n", refBest.Round(time.Millisecond))
	fmt.Printf("  speed-up               : %10.1fx  (mode %s, %d buckets, %d workers)\n",
		float64(refBest)/float64(engineBest), stats.Mode, stats.Buckets, stats.Workers)

	// TopK timing: the heap path should beat a full sort when k is small.
	k := 100
	t0 := time.Now()
	res := s.Sort(rows, sortengine.Options{TopK: k, Workers: workers})
	topD := time.Since(t0)

	t0 = time.Now()
	full := s.Sort(rows, sortengine.Options{Workers: workers})
	fullD := time.Since(t0)
	if !equalOrder(res.Order, full.Order[:k]) {
		fmt.Printf("  TOPK %d MISMATCH on benchmark data\n", k)
	}
	fmt.Printf("  top-%d via heap         : %10v   (full sort %v, %.1fx faster)\n",
		k, topD.Round(time.Millisecond), fullD.Round(time.Millisecond), float64(fullD)/float64(topD))
}
