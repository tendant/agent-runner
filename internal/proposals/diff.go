package proposals

import (
	"fmt"
	"strings"
)

// maxDiffLines bounds the line-by-line comparison; past it the diff is
// shown as the whole old and new file (still correct, just long).
const maxDiffLines = 3000

// Diff is a unified diff of old to new with three lines of context, for a
// person to review.
func Diff(oldText, newText, name string) string {
	a, b := splitLines(oldText), splitLines(newText)
	ops := diffOps(a, b)
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", name, name)
	const ctx = 3
	// Group the ops into hunks: runs of changes with up to ctx equal lines
	// around them.
	i := 0
	for i < len(ops) {
		for i < len(ops) && ops[i].kind == ' ' {
			i++
		}
		if i == len(ops) {
			break
		}
		start := max(0, i-ctx)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*ctx {
				end = min(len(ops), end+ctx)
				break
			}
			end = run
		}
		aStart, bStart := ops[start].a, ops[start].b
		var aN, bN int
		var body strings.Builder
		for _, op := range ops[start:end] {
			body.WriteByte(op.kind)
			body.WriteString(op.text)
			body.WriteByte('\n')
			if op.kind != '+' {
				aN++
			}
			if op.kind != '-' {
				bN++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aN), hunkRange(bStart, bN))
		out.WriteString(body.String())
		i = end
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

type op struct {
	kind byte // ' ', '-', '+'
	text string
	a, b int // line indexes (0-based) in old and new where this op is
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffOps is an LCS edit script of a to b.
func diffOps(a, b []string) []op {
	// Common prefix and suffix first: most proposals change a little.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []op
	for i := 0; i < pre; i++ {
		ops = append(ops, op{' ', a[i], i, i})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(ma) > maxDiffLines || len(mb) > maxDiffLines {
		for i, l := range ma {
			ops = append(ops, op{'-', l, pre + i, pre})
		}
		for j, l := range mb {
			ops = append(ops, op{'+', l, pre + len(ma), pre + j})
		}
	} else {
		n, m := len(ma), len(mb)
		lcs := make([][]int32, n+1)
		for i := range lcs {
			lcs[i] = make([]int32, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && ma[i] == mb[j]:
				ops = append(ops, op{' ', ma[i], pre + i, pre + j})
				i++
				j++
			case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
				ops = append(ops, op{'-', ma[i], pre + i, pre + j})
				i++
			default:
				ops = append(ops, op{'+', mb[j], pre + i, pre + j})
				j++
			}
		}
	}
	for k := 0; k < suf; k++ {
		ia, ib := len(a)-suf+k, len(b)-suf+k
		ops = append(ops, op{' ', a[ia], ia, ib})
	}
	return ops
}
