// Command jet is Lightning Jet 2.0's command-line interface and daemon.
//
// It is in early development: nothing here reads from or acts on a node yet.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command and returns the process exit code: 0 on success,
// 2 on a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, "jet", version)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "jet: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Lightning Jet 2.0 (in development; it does not read from or act on a node yet)

Usage:
  jet <command>

Commands:
  version   Print the version
  help      Show this help
`)
}
