package ls

import "strings"

// zigFormatText re-indents Zig source by brace depth while preserving every other token.
// TypeScript's AST formatter normalizes spacing for a different grammar (it rewrites `Foo{` to
// `Foo {`, drops the space in `[]const []const u8`, ...), so it is not used for Zig. This keeps the
// author's spacing and only fixes leading indentation and trailing whitespace, which is idempotent.
func zigFormatText(text string, tabSize int, insertSpaces bool) string {
	unit := "\t"
	if insertSpaces {
		if tabSize <= 0 {
			tabSize = 4
		}
		unit = strings.Repeat(" ", tabSize)
	}
	lines := strings.Split(text, "\n")
	depth := 0
	var b strings.Builder
	b.Grow(len(text) + 16)
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		content := strings.TrimRight(strings.TrimLeft(line, " \t"), " \t\r")
		if content == "" {
			continue
		}
		lineDepth := depth
		if strings.HasPrefix(content, "}") {
			lineDepth--
		}
		if lineDepth < 0 {
			lineDepth = 0
		}
		b.WriteString(strings.Repeat(unit, lineDepth))
		b.WriteString(content)
		depth += zigBraceDelta(content)
		if depth < 0 {
			depth = 0
		}
	}
	return b.String()
}

// zigBraceDelta returns the net `{`/`}` count of a line, ignoring line comments and string literals.
func zigBraceDelta(line string) int {
	delta := 0
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return delta
		case c == '\\' && i+1 < len(line) && line[i+1] == '\\':
			// Multiline string literal (`\\...`): the rest of the line is string content.
			return delta
		case c == '"' || c == '\'':
			quote := c
			i++
			for i < len(line) {
				if line[i] == '\\' {
					i += 2
					continue
				}
				if line[i] == quote {
					i++
					break
				}
				i++
			}
		case c == '{':
			delta++
			i++
		case c == '}':
			delta--
			i++
		default:
			i++
		}
	}
	return delta
}
