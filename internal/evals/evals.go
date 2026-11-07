// Package evals is the eval set (PROACTIVE_DESIGN.md §11): past tasks the
// operator chose, each a message and checks, kept in MEMORY_DIR/evals/. A
// proposal is judged by replaying the set twice, with the current
// instructions and with the proposed ones, and comparing what passed.
//
// An eval file:
//
//	---
//	file: hello.go
//	command: go vet ./...
//	reply: gofmt
//	---
//	Add hello.go that prints hello, and commit it.
//
// file: must exist in the workspace afterwards; command: must exit 0 there
// (run in the replay's sandbox); reply: must appear in the agent's reply.
// Each may be given more than once.
package evals

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-runner/agent-runner/internal/agent"
	tmpl "github.com/agent-runner/agent-runner/internal/template"
)

// Dir is the eval set's folder in the memory dir. It is a subdirectory, so
// a session's memory merge never writes it: the operator's alone.
const Dir = "evals"

// MaxEvals bounds a set: each is two replays per proposal.
const MaxEvals = 10

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Eval is one task of the set.
type Eval struct {
	Name     string
	Message  string
	Checks   []agent.EvalCheck
	Problems []string
}

// Parse reads an eval file.
func Parse(name, content string) Eval {
	e := Eval{Name: name}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	body := content
	if rest, ok := strings.CutPrefix(content, "---\n"); ok {
		if head, after, ok := strings.Cut(rest, "\n---"); ok {
			body = after
			if nl := strings.IndexByte(body, '\n'); nl >= 0 {
				body = body[nl+1:]
			} else {
				body = ""
			}
			for _, line := range strings.Split(head, "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				k, v, ok := strings.Cut(line, ":")
				k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
				switch {
				case !ok || v == "":
					e.Problems = append(e.Problems, "not key: value: "+strings.TrimSpace(line))
				case k == "file" || k == "command" || k == "reply":
					e.Checks = append(e.Checks, agent.EvalCheck{Kind: k, Arg: v})
				default:
					e.Problems = append(e.Problems, "unknown check "+k+" (file, command, reply)")
				}
			}
		}
	}
	e.Message = strings.TrimSpace(body)
	if e.Message == "" {
		e.Problems = append(e.Problems, "no task: the message goes after the front matter")
	}
	if len(e.Checks) == 0 {
		e.Problems = append(e.Problems, "no checks: nothing to compare")
	}
	return e
}

// Usable reports whether e can be replayed.
func (e Eval) Usable() bool { return e.Message != "" && len(e.Checks) > 0 }

// List reads the eval set, sorted by name.
func List(memoryDir string) ([]Eval, error) {
	entries, err := os.ReadDir(filepath.Join(memoryDir, Dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Eval
	for _, en := range entries {
		name, ok := strings.CutSuffix(en.Name(), ".md")
		if !ok || !en.Type().IsRegular() || !nameRe.MatchString(name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(memoryDir, Dir, en.Name()))
		if err != nil {
			continue
		}
		out = append(out, Parse(name, string(data)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Path is an eval's file.
func Path(memoryDir, name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("an eval's name is lowercase letters, digits and dashes: %q", name)
	}
	return filepath.Join(memoryDir, Dir, name+".md"), nil
}

// Write saves an eval as an operator edit (its own memory commit).
func Write(memoryDir, name, content string) (Eval, error) {
	path, err := Path(memoryDir, name)
	if err != nil {
		return Eval{}, err
	}
	e := Parse(name, content)
	if !e.Usable() {
		return e, errors.New(strings.Join(e.Problems, "; "))
	}
	if all, _ := List(memoryDir); len(all) >= MaxEvals {
		if _, err := os.Stat(path); err != nil {
			return e, fmt.Errorf("the set has %d evals already (each is two replays per proposal): remove one first", MaxEvals)
		}
	}
	return e, tmpl.WriteInstructions(memoryDir, path, strings.TrimSpace(content)+"\n", "memory: eval "+name+" set by the operator (chat)")
}

// Remove deletes an eval, committing that.
func Remove(memoryDir, name string) (bool, error) {
	path, err := Path(memoryDir, name)
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, tmpl.CommitInstructions(memoryDir, path, "memory: eval "+name+" removed by the operator (chat)")
}
