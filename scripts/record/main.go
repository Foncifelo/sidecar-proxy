// Command record captures a provider's real responses into internal/providers/testdata
// so adapters can be tested against recordings instead of the live API.
// It runs only by hand (make record PROVIDER=...), never in CI.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	provider := flag.String("provider", "", "provider id (e.g. together)")
	out := flag.String("out", "", "output directory")
	flag.Parse()
	if *provider == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: record -provider <id> -out <dir>")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "not implemented yet: records chat.json and chat.stream.sse, scrubbing keys")
	os.Exit(1)
}
