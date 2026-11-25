// Command verifyproto checks that a GTFS-Realtime feed message carries the
// static key of the static GTFS dataset the consumer is using.
//
// The key is read from feed_header.feed_version (GTFS-RT v3.0 field 4), which
// older bindings carry as an unknown field but is still present on the wire.
// The expected key can be given directly (-key) or resolved from a static ZIP
// the same way the realtime producer resolves it (-gtfs-file).
//
// Exit code 0 means the proto matches; any mismatch or parse failure exits
// non-zero with a message on stderr, so a handover consumer can refuse a
// stale/wrong proto instead of silently mixing datasets.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/MKuranowski/WarsawGTFS/realtime/gtfs"
	"github.com/MKuranowski/WarsawGTFS/realtime/rtstatic"
)

func main() {
	protoPath := flag.String("proto", "", "path to the GTFS-Realtime proto file (required)")
	wantKey := flag.String("key", "", "expected static key")
	gtfsPath := flag.String("gtfs-file", "", "static GTFS ZIP to derive the expected key from")
	flag.Parse()

	if *protoPath == "" || (*wantKey == "" && *gtfsPath == "") {
		fmt.Fprintln(os.Stderr, "usage: verifyproto -proto positions.pb (-key KEY | -gtfs-file gtfs.zip)")
		os.Exit(2)
	}

	b, err := os.ReadFile(*protoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading proto: %v\n", err)
		os.Exit(1)
	}

	if *wantKey == "" {
		g, err := gtfs.NewGtfsFromFile(*gtfsPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "opening static GTFS: %v\n", err)
			os.Exit(1)
		}
		defer g.Close()
		*wantKey, err = g.StaticKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolving static key: %v\n", err)
			os.Exit(1)
		}
	}

	msg, gotKey, err := rtstatic.Verify(b, *wantKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "VERIFY FAILED: %v\n", err)
		os.Exit(1)
	}

	ts := msg.GetHeader().GetTimestamp()
	fmt.Printf("OK: proto static key %q matches expected key\n", gotKey)
	fmt.Printf("    feed header timestamp: %d\n", ts)
}
