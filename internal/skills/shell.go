package skills

import (
	"regexp"
	"strings"
)

// simpleCmd is one simple command of a shell line, after quote removal.
type simpleCmd struct {
	// assigns are the NAME=VALUE words before the command word.
	assigns []string
	words   []string
	// writes are the targets of the redirections that write a file.
	writes []string
}

// shellLine is a shell line split into simple commands. unsafe names a form
// that the guard cannot check, for example a command substitution.
type shellLine struct {
	cmds   []simpleCmd
	unsafe string
}

var assignRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// parseShell splits a shell line at ;, &, &&, ||, |, parentheses and line
// ends into simple commands. It removes quotes and escapes, records the
// targets of >, >>, >| and &>, skips the bodies of here-documents and
// comments, and marks command substitution, backticks and process
// substitution as unsafe.
func parseShell(line string) shellLine {
	var out shellLine
	var cur simpleCmd
	var word strings.Builder
	inWord := false
	redirect := false // the next word is the target of a write redirection
	readRedirect := false
	var heredocs []string // delimiters of the here-documents of this line

	endWord := func() {
		if !inWord {
			return
		}
		w := word.String()
		word.Reset()
		inWord = false
		switch {
		case redirect:
			redirect = false
			if !strings.HasPrefix(w, "&") {
				cur.writes = append(cur.writes, w)
			}
		case readRedirect:
			readRedirect = false
		case len(cur.words) == 0 && assignRE.MatchString(w):
			cur.assigns = append(cur.assigns, w)
		default:
			cur.words = append(cur.words, w)
		}
	}
	endCmd := func() {
		endWord()
		// The braces of a group are words of their own.
		for len(cur.words) > 0 && (cur.words[0] == "{" || cur.words[0] == "!") {
			cur.words = cur.words[1:]
		}
		for len(cur.words) > 0 && cur.words[len(cur.words)-1] == "}" {
			cur.words = cur.words[:len(cur.words)-1]
		}
		if len(cur.words) > 0 || len(cur.assigns) > 0 || len(cur.writes) > 0 {
			out.cmds = append(out.cmds, cur)
		}
		cur = simpleCmd{}
	}
	unsafe := func(why string) {
		if out.unsafe == "" {
			out.unsafe = why
		}
	}

	n := len(line)
	for i := 0; i < n; i++ {
		c := line[i]
		switch {
		case c == '\\':
			if i+1 < n {
				if line[i+1] == '\n' {
					i++
					continue
				}
				word.WriteByte(line[i+1])
				inWord = true
				i++
			}
		case c == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				unsafe("an open quote")
				word.WriteString(line[i+1:])
				i = n
			} else {
				word.WriteString(line[i+1 : i+1+j])
				i += j + 1
			}
			inWord = true
		case c == '"':
			inWord = true
			i++
			for ; i < n && line[i] != '"'; i++ {
				switch {
				case line[i] == '\\' && i+1 < n && strings.IndexByte("\"\\$`\n", line[i+1]) >= 0:
					if line[i+1] != '\n' {
						word.WriteByte(line[i+1])
					}
					i++
				case line[i] == '`':
					unsafe("a command substitution")
				case line[i] == '$' && i+1 < n && line[i+1] == '(':
					unsafe("a command substitution")
					word.WriteByte('$')
				default:
					word.WriteByte(line[i])
				}
			}
			if i >= n {
				unsafe("an open quote")
			}
		case c == '`':
			unsafe("a command substitution")
		case c == '$' && i+1 < n && line[i+1] == '(':
			unsafe("a command substitution")
			word.WriteByte('$')
			inWord = true
		case c == '#' && !inWord:
			for i < n && line[i] != '\n' {
				i++
			}
			i--
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '\n':
			endCmd()
			if len(heredocs) > 0 {
				i = skipHeredocs(line, i+1, heredocs) - 1
				heredocs = nil
			}
		case c == ';' || c == '(' || c == ')':
			endCmd()
		case c == '|':
			endCmd()
			if i+1 < n && (line[i+1] == '|' || line[i+1] == '&') {
				i++
			}
		case c == '&':
			if i+1 < n && line[i+1] == '>' {
				endWord()
				i++
				if i+1 < n && line[i+1] == '>' {
					i++
				}
				redirect = true
				continue
			}
			endCmd()
			if i+1 < n && line[i+1] == '&' {
				i++
			}
		case c == '>':
			if i+1 < n && line[i+1] == '(' {
				unsafe("a process substitution")
			}
			// A word of digits right before > is a file descriptor.
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			} else {
				endWord()
			}
			if i+1 < n && (line[i+1] == '>' || line[i+1] == '|') {
				i++
			}
			if i+1 < n && line[i+1] == '&' {
				// >&2: a dup, no file.
				i++
				for i+1 < n && (line[i+1] >= '0' && line[i+1] <= '9' || line[i+1] == '-') {
					i++
				}
				continue
			}
			redirect = true
		case c == '<':
			if i+1 < n && line[i+1] == '(' {
				unsafe("a process substitution")
			}
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			} else {
				endWord()
			}
			switch {
			case strings.HasPrefix(line[i:], "<<<"):
				i += 2
				readRedirect = true
			case strings.HasPrefix(line[i:], "<<"):
				i++ // i is at the last <
				if i+1 < n && line[i+1] == '-' {
					i++
				}
				for i+1 < n && (line[i+1] == ' ' || line[i+1] == '\t') {
					i++
				}
				j := i + 1
				for j < n && strings.IndexByte(" \t\n;&|<>()", line[j]) < 0 {
					j++
				}
				delim := strings.NewReplacer("'", "", `"`, "", `\`, "").Replace(line[i+1 : j])
				heredocs = append(heredocs, delim)
				i = j - 1
			case i+1 < n && line[i+1] == '&':
				i++
				for i+1 < n && (line[i+1] >= '0' && line[i+1] <= '9' || line[i+1] == '-') {
					i++
				}
			default:
				readRedirect = true
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCmd()
	return out
}

// skipHeredocs skips the bodies of the here-documents that start at i and
// gives the position after the last delimiter line.
func skipHeredocs(line string, i int, delims []string) int {
	for _, d := range delims {
		for i < len(line) {
			end := strings.IndexByte(line[i:], '\n')
			var l string
			if end < 0 {
				l, i = line[i:], len(line)
			} else {
				l, i = line[i:i+end], i+end+1
			}
			if strings.TrimLeft(l, "\t") == d {
				break
			}
		}
	}
	return i
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
