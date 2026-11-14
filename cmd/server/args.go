package main

import (
	"fmt"
	"io"
	"runtime/debug"
)

const usageText = `agent-runner: runs AI coding agents in sandboxes, driven by chat and HTTP.

USAGE:
  agent-runner [serve]      start the server (the default); configured by .env and the environment
  agent-runner version      print the build (also --version, -v)
  agent-runner help         show this help (also --help, -h)

.env.example lists every setting; docs/ has the rest.
`

// command is what the command line asks for.
type command int

const (
	cmdServe command = iota
	cmdRunSession
	cmdExit
)

// parseArgs decides what to run before anything is loaded or written: an
// argument the server does not know must not start it, since starting
// creates state (state/, audit/, a job secret) under the current directory.
// For cmdExit, code is the process exit code.
func parseArgs(args []string, out io.Writer) (cmd command, code int) {
	if len(args) == 0 {
		return cmdServe, 0
	}
	switch args[0] {
	case "serve":
		if len(args) == 1 {
			return cmdServe, 0
		}
		fmt.Fprintf(out, "agent-runner serve takes no arguments (got %q); it is configured by .env and the environment.\n", args[1:])
		return cmdExit, 2
	case "run-session":
		return cmdRunSession, 0
	case "version", "--version", "-version", "-v":
		fmt.Fprintln(out, versionString())
		return cmdExit, 0
	case "help", "--help", "-help", "-h":
		fmt.Fprint(out, usageText)
		return cmdExit, 0
	default:
		fmt.Fprintf(out, "unknown argument %q\n\n", args[0])
		fmt.Fprint(out, usageText)
		return cmdExit, 2
	}
}

// versionString is the commit the binary was built from (with -dirty when
// the checkout had changes), from the VCS stamp Go embeds, plus the Go
// version and buildTime when the build set it.
func versionString() string {
	rev, dirty, goVersion := "unknown", "", ""
	if info, ok := debug.ReadBuildInfo(); ok {
		goVersion = info.GoVersion
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
				if len(rev) > 7 {
					rev = rev[:7]
				}
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
	}
	v := "agent-runner " + rev + dirty
	if goVersion != "" {
		v += " (" + goVersion + ")"
	}
	if buildTime != "unknown" {
		v += " built " + buildTime
	}
	return v
}
