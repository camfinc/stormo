package sched

import (
	"fmt"
	"strings"
)

// unifiedDiff is old → new as a unified diff of lines with 3 lines of context, headed
// "--- previous" / "+++ current" as Hermes' monitor writes it ("" when equal). Inputs too large
// for the line-by-line comparison come out as one hunk replacing everything.
func unifiedDiff(old, new string) string {
	a, b := splitLines(old), splitLines(new)
	ops := lineOps(a, b)
	const context = 3
	var out strings.Builder
	out.WriteString("--- previous\n+++ current")
	changed := false
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		changed = true
		// A hunk: from context lines before the change to context lines after the last change
		// that is within 2*context of the next one.
		start := max(0, i-context)
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
			if run == len(ops) || run-end > 2*context {
				end = min(len(ops), end+context)
				break
			}
			end = run
		}
		aStart, bStart, aLen, bLen := ops[start].a, ops[start].b, 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				aLen++
			}
			if op.kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&out, "\n@@ -%s +%s @@", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		for _, op := range ops[start:end] {
			out.WriteByte('\n')
			out.WriteByte(op.kind)
			out.WriteString(op.text)
		}
		i = end
	}
	if !changed {
		return ""
	}
	return out.String()
}

// hunkRange is a hunk's "start,length" (1-based; difflib's form for an empty range).
func hunkRange(start, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", start)
	case 1:
		return fmt.Sprint(start + 1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type lineOp struct {
	kind byte // ' ', '-', '+'
	text string
	a, b int // index of the line in old and new (where it would be, for an insert or delete)
}

// maxCells bounds the comparison's table (lines old × lines new).
const maxCells = 4_000_000

// lineOps is an edit script turning a into b: a longest common subsequence of lines, with the
// common prefix and suffix taken off first.
func lineOps(a, b []string) []lineOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]lineOp, 0, len(a)+len(b))
	for i := 0; i < pre; i++ {
		ops = append(ops, lineOp{' ', a[i], i, i})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(ma)*len(mb) > maxCells {
		for i, l := range ma {
			ops = append(ops, lineOp{'-', l, pre + i, pre})
		}
		for j, l := range mb {
			ops = append(ops, lineOp{'+', l, pre + len(ma), pre + j})
		}
	} else {
		// lcs[i][j]: the common subsequence's length of ma[i:] and mb[j:]
		w := len(mb) + 1
		lcs := make([]int32, (len(ma)+1)*w)
		for i := len(ma) - 1; i >= 0; i-- {
			for j := len(mb) - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
				} else {
					lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
				}
			}
		}
		i, j := 0, 0
		for i < len(ma) || j < len(mb) {
			switch {
			case i < len(ma) && j < len(mb) && ma[i] == mb[j]:
				ops = append(ops, lineOp{' ', ma[i], pre + i, pre + j})
				i, j = i+1, j+1
			case i < len(ma) && (j == len(mb) || lcs[(i+1)*w+j] >= lcs[i*w+j+1]):
				ops = append(ops, lineOp{'-', ma[i], pre + i, pre + j})
				i++
			default:
				ops = append(ops, lineOp{'+', mb[j], pre + i, pre + j})
				j++
			}
		}
	}
	for k := 0; k < suf; k++ {
		ia, ib := len(a)-suf+k, len(b)-suf+k
		ops = append(ops, lineOp{' ', a[ia], ia, ib})
	}
	return ops
}
