// Command sortweb serves the sorting-operator demo page.
//
//	go run ./cmd/sortweb -addr :8080
//
// The page lets you edit the sort columns, generates a race-season dataset,
// shows the encoded key samples, the per-phase timings and a side-by-side
// speed comparison against a column-by-column stable sort.
package main

import (
	"flag"
	"log"
	"net/http"

	"pigeonsort/web"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	log.Printf("sorting demo: http://localhost%s", *addr)
	srv := &http.Server{Addr: *addr, Handler: web.NewHandler()}
	log.Fatal(srv.ListenAndServe())
}
