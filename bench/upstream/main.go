// Command upstream is the slow service the HTTP client benchmarks call:
// /delay?ms=N answers after N milliseconds. /hello answers at once and is the
// plain Go baseline for the hello benchmark.
package main

import (
	"flag"
	"log"
	"net/http"
	"strconv"
	"time"
)

func main() {
	addr := flag.String("addr", ":9100", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/delay", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("upstream"))
	})
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("Hello, World!"))
	})
	log.Fatal(http.ListenAndServe(*addr, mux))
}
