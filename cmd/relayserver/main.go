// relayserver is the relay server entry point.
//
// Usage:
//
//	relayserver -listen :19132
package main

import (
	"flag"
	"log"

	"relaynet/internal/relay"
)

func main() {
	listen := flag.String("listen", ":19132", "UDP listen address")
	flag.Parse()

	srv, err := relay.New(*listen)
	if err != nil {
		log.Fatalf("start server: %v", err)
	}
	srv.Serve()
}
