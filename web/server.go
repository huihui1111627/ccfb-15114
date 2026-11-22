// Package web serves the demo page and its JSON endpoints.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"runtime"
	"strings"
	"time"

	"pigeonsort/internal/harness"
	"pigeonsort/sortengine"
)

//go:embed index.html assets/*
var content embed.FS

// NewHandler returns the demo site handler.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/meta", handleMeta)
	mux.HandleFunc("/api/sort", handleSort)
	mux.Handle("/", http.FileServer(http.FS(mustFS())))
	return mux
}

func mustFS() fs.FS {
	sub, err := fs.Sub(content, ".")
	if err != nil {
		panic(err)
	}
	return sub
}

// ColumnReq is one sort-column definition submitted by the page.
type ColumnReq struct {
	Name   string `json:"name"`
	Expr   string `json:"expr"`
	Kind   string `json:"kind"`
	Dir    string `json:"dir"`
	Nulls  string `json:"nulls"`
	Pinyin bool   `json:"pinyin"`
}

// SortReq is the /api/sort request body.
type SortReq struct {
	Columns []ColumnReq `json:"columns"`
	Rows    int         `json:"rows"`
	Seed    int64       `json:"seed"`
	TopK    int         `json:"topK"`
	Workers int         `json:"workers"`
	Sample  int         `json:"sample"`
	Compare bool        `json:"compare"`
}

// response types
type metaResponse struct {
	Fields      []fieldInfo `json:"fields"`
	Expressions []exprInfo  `json:"expressions"`
	LoftNames   []string    `json:"loftNames"`
	GOMAXPROCS  int         `json:"gomaxprocs"`
	GoVersion   string      `json:"goVersion"`
}

type fieldInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Desc string `json:"desc"`
}

type exprInfo struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
	Kind string `json:"kind"`
}

func handleMeta(w http.ResponseWriter, r *http.Request) {
	resp := metaResponse{
		LoftNames:  harness.LoftNames,
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		GoVersion:  runtime.Version(),
	}
	for _, f := range harness.Fields {
		resp.Fields = append(resp.Fields, fieldInfo{Name: f.Name, Kind: f.Kind.String(), Desc: f.Desc})
	}
	for _, e := range harness.ExprCatalog {
		resp.Expressions = append(resp.Expressions, exprInfo{Name: e.Name, Expr: e.Src, Kind: e.Kind.String()})
	}
	writeJSON(w, resp)
}

// sortResponse is what the page renders.
type sortResponse struct {
	OK       bool                           `json:"ok"`
	Error    string                         `json:"error,omitempty"`
	Columns  []sortengine.ColumnDescription `json:"columns"`
	Stats    sortengine.Stats               `json:"stats"`
	Samples  []sortengine.KeySample         `json:"samples"`
	KeyLen   float64                        `json:"keyLen"`
	Ordering []orderRow                     `json:"ordering"`
	Compare  *compareResult                 `json:"compare,omitempty"`
	DataMs   float64                        `json:"dataMs"`
}

type orderRow struct {
	Rank   int      `json:"rank"`
	Values []string `json:"values"`
}

type compareResult struct {
	EngineNs        int64   `json:"engineNs"`
	ReferenceNs     int64   `json:"referenceNs"`
	Speedup         float64 `json:"speedup"`
	EngineBestNs    int64   `json:"engineBestNs"`
	ReferenceBestNs int64   `json:"referenceBestNs"`
	Rows            int     `json:"rows"`
	VerifyOK        bool    `json:"verifyOk"`
}

func handleSort(w http.ResponseWriter, r *http.Request) {
	var req SortReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, sortResponse{Error: "请求解析失败: " + err.Error()})
		return
	}
	resp := runSort(req)
	writeJSON(w, resp)
}

func runSort(req SortReq) sortResponse {
	if req.Rows <= 0 {
		req.Rows = 100000
	}
	if req.Rows > 3000000 {
		req.Rows = 3000000
	}
	if req.Sample <= 0 {
		req.Sample = 8
	}
	if req.Sample > 30 {
		req.Sample = 30
	}
	if req.Seed == 0 {
		req.Seed = time.Now().UnixNano()
	}
	schema := harness.Schema()
	var cols []sortengine.Column
	for i, c := range req.Columns {
		col, err := sortengine.ParseColumn(c.Name, c.Kind, c.Expr,
			sortengine.ParseDirection(c.Dir), sortengine.ParseNullsOrder(c.Nulls), c.Pinyin)
		if err != nil {
			return sortResponse{Error: fmt.Sprintf("第 %d 列: %v", i+1, err)}
		}
		if strings.TrimSpace(c.Name) == "" {
			col.Name = fmt.Sprintf("c%d", i+1)
		}
		cols = append(cols, col)
	}
	if len(cols) == 0 {
		return sortResponse{Error: "至少需要一列排序键"}
	}
	spec := sortengine.Spec{Columns: cols, Parallel: true, MinParallelRows: 20000}
	s, err := sortengine.New(spec, schema)
	if err != nil {
		return sortResponse{Error: err.Error()}
	}
	rows := harness.GenerateRows(rand.New(rand.NewSource(req.Seed)), req.Rows)
	resp := sortResponse{OK: true, Columns: s.ColumnDescriptions()}
	res := s.Sort(rows, sortengine.Options{TopK: req.TopK, Samples: req.Sample, Workers: req.Workers})
	resp.Stats = res.Stats
	resp.Samples = res.Samples
	resp.KeyLen = res.Stats.AvgKeyLen
	if resp.Samples == nil {
		resp.Samples = []sortengine.KeySample{}
	}
	// First rows of the sorted table, as display strings.
	limit := 20
	order := res.Order
	if len(order) > limit {
		order = order[:limit]
	}
	for rank, idx := range order {
		row := rows[idx]
		vals := make([]string, len(row))
		for i, v := range row {
			vals[i] = v.String()
		}
		resp.Ordering = append(resp.Ordering, orderRow{Rank: rank + 1, Values: vals})
	}
	if req.Compare {
		// Warm both paths so the comparison measures steady state.
		s.Sort(rows, sortengine.Options{Workers: req.Workers})
		s.ReferenceOrder(rows)
		var engineBest, refBest time.Duration
		var verified bool
		for i := 0; i < 2; i++ {
			t0 := time.Now()
			got := s.Sort(rows, sortengine.Options{Workers: req.Workers})
			d := time.Since(t0)
			if engineBest == 0 || d < engineBest {
				engineBest = d
			}
			t0 = time.Now()
			want := s.ReferenceOrder(rows)
			d = time.Since(t0)
			if refBest == 0 || d < refBest {
				refBest = d
			}
			verified = equalOrder(got.Order, want)
		}
		resp.Compare = &compareResult{
			EngineNs:    engineBest.Nanoseconds(),
			ReferenceNs: refBest.Nanoseconds(),
			Speedup:     float64(refBest) / float64(engineBest),
			Rows:        len(rows),
			VerifyOK:    verified,
		}
	}
	return resp
}

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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode: %v", err)
	}
}
