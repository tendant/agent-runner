package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		cmd  command
		code int
		says string
	}{
		{nil, cmdServe, 0, ""},
		{[]string{"serve"}, cmdServe, 0, ""},
		{[]string{"run-session", "--session", "s1"}, cmdRunSession, 0, ""},
		{[]string{"--version"}, cmdExit, 0, "agent-runner "},
		{[]string{"version"}, cmdExit, 0, "agent-runner "},
		{[]string{"-v"}, cmdExit, 0, "agent-runner "},
		{[]string{"--help"}, cmdExit, 0, "USAGE"},
		{[]string{"-h"}, cmdExit, 0, "USAGE"},
		{[]string{"--port=9000"}, cmdExit, 2, "unknown argument"},
		{[]string{"serve", "extra"}, cmdExit, 2, "takes no arguments"},
	} {
		var out bytes.Buffer
		cmd, code := parseArgs(tc.args, &out)
		if cmd != tc.cmd || code != tc.code || !strings.Contains(out.String(), tc.says) {
			t.Errorf("%v: cmd=%v code=%d out=%q", tc.args, cmd, code, out.String())
		}
	}
}

// The reason for parseArgs: --version used to start the server, which
// created state/ and audit/ (and a job secret) in the current directory.
func TestVersionWritesNothing(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	for _, a := range []string{"--version", "--help", "--bogus"} {
		parseArgs([]string{a}, &bytes.Buffer{})
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("parsing arguments wrote %d entries to the working directory", len(entries))
	}
}
