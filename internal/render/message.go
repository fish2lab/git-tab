package render

import (
	"regexp"
	"strings"
)

// Placing trailers in a commit message, after trusty-mpm's
// commit_trailers.rs:
//
//   - git reads trailers from the last paragraph only, and never from the
//     first (the title). A message already ending in trailers
//     (Signed-off-by, Co-Authored-By) gets ours in that same paragraph, or
//     git would stop seeing theirs.
//   - A trailing run of comment lines is not the end of the message: the
//     insert goes after the last content line, above it.
//   - `git commit --verbose` appends a diff below a scissors line that
//     git discards; the scissors bound the insert.

const scissors = "------------------------ >8 ------------------------"

var trailerLine = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*:[ \t]`)

type parts struct {
	content []string // message lines up to the last real one
	tail    []string // trailing comments, scissors and what follows
	nl      bool     // the message ended in a newline
}

func split(msg, cc string) parts {
	nl := strings.HasSuffix(msg, "\n")
	lines := strings.Split(strings.TrimSuffix(msg, "\n"), "\n")
	if msg == "" {
		lines = nil
	}
	end := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(l, cc) && strings.TrimSpace(strings.TrimPrefix(l, cc)) == scissors {
			end = i
			break
		}
	}
	last := -1
	for i := 0; i < end; i++ {
		if !comment(lines[i], cc) && strings.TrimSpace(lines[i]) != "" {
			last = i
		}
	}
	return parts{content: lines[:last+1], tail: lines[last+1:], nl: nl}
}

func comment(l, cc string) bool { return cc != "" && strings.HasPrefix(l, cc) }

func (p parts) join() string {
	all := append(append([]string{}, p.content...), p.tail...)
	s := strings.Join(all, "\n")
	if p.nl || len(p.tail) == 0 {
		s += "\n"
	}
	return s
}

// paragraphs are the start indexes of the content's paragraphs.
func paragraphs(content []string) []int {
	var starts []int
	inside := false
	for i, l := range content {
		blank := strings.TrimSpace(l) == ""
		if !blank && !inside {
			starts = append(starts, i)
		}
		inside = !blank
	}
	return starts
}

// trailerBlock says whether lines are a trailer paragraph as git reads it:
// a trailer first, then trailers or their indented continuations.
func trailerBlock(lines []string) bool {
	if len(lines) == 0 || !trailerLine.MatchString(lines[0]) {
		return false
	}
	for _, l := range lines[1:] {
		if !trailerLine.MatchString(l) && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			return false
		}
	}
	return true
}

// HasContent says whether a message has anything but comments, blank lines
// and a --verbose diff: git aborts a commit whose message has nothing.
func HasContent(msg, cc string) bool { return len(split(msg, cc).content) > 0 }

// Apply puts art above the message's trailer paragraph and trailers at its
// end, starting one when the message has none.
func Apply(msg string, art, trailers []string, cc string) string {
	p := split(msg, cc)
	starts := paragraphs(p.content)
	var head, block []string
	if n := len(starts); n > 1 && trailerBlock(p.content[starts[n-1]:]) {
		head, block = p.content[:starts[n-1]], p.content[starts[n-1]:]
		head = trimBlank(head)
	} else {
		head = p.content
	}
	out := append([]string{}, head...)
	if len(art) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, art...)
	}
	if len(out) > 0 {
		out = append(out, "")
	}
	out = append(out, block...)
	out = append(out, trailers...)
	p.content = out
	if len(p.tail) > 0 && strings.TrimSpace(p.tail[0]) != "" {
		p.tail = append([]string{""}, p.tail...)
	}
	return p.join()
}

func trimBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func ours(l string) bool {
	for _, k := range Keys {
		if strings.HasPrefix(l, k+":") {
			return true
		}
	}
	return false
}

// Strip removes a receipt this package wrote: its trailers from the
// message's last paragraph and the receipt drawing, if any.
func Strip(msg, cc string) string {
	p := split(msg, cc)
	content := p.content
	if starts := paragraphs(content); len(starts) > 1 {
		last := starts[len(starts)-1]
		if trailerBlock(content[last:]) {
			var kept []string
			for _, l := range content[last:] {
				if !ours(l) {
					kept = append(kept, l)
				}
			}
			content = append(trimBlank(content[:last]), "")
			content = append(content, kept...)
			content = trimBlank(content)
		}
	}
	// The receipt drawing is the paragraph holding its header mark.
	starts := paragraphs(content)
	var out []string
	if len(starts) > 0 {
		out = append(out, content[:starts[0]]...)
	}
	for i, s := range starts {
		end := len(content)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		if para := content[s:end]; !strings.Contains(strings.Join(para, "\n"), ReceiptMark) {
			out = append(out, para...)
		}
	}
	p.content = trimBlank(out)
	if len(p.content) == 0 {
		p.content = nil
	}
	return p.join()
}
