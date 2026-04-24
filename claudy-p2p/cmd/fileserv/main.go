// Tiny static file server. Used to hand out cross-compiled binaries.
package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", ":8000", "listen address")
	dir := flag.String("dir", ".", "directory to serve")
	flag.Parse()

	log.Printf("serving %s on %s", *dir, *addr)
	log.Fatal(http.ListenAndServe(*addr, http.FileServer(http.Dir(*dir))))
}
