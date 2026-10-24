// Command culprit investigates production incidents.
package main

import (
	"fmt"
	"os"
)

const usage = `culprit: AI incident investigator

Usage:
  culprit investigate [flags]   investigate one incident and print a report
  culprit serve [flags]         run the API server for the web UI and webhooks
  culprit eval [flags]          score Culprit against fixture scenarios

Run "culprit <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "investigate":
		err = runInvestigate(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "eval":
		err = runEval(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "culprit:", err)
		os.Exit(1)
	}
}
