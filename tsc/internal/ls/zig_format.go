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
	// Depths at which a `const`/`var` declaration started and is still awaiting its `;`.
	pending := map[int]bool{}
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

		newDepth := depth + zigBraceDelta(content)

		// A `const`/`var` declaration must end with `;` (unlike `fn` bodies and `comptime` blocks,
		// which are terminated by `}`); track where each one starts so a missing `;` can be added.
		if keyword := zigDeclarationKeyword(content); keyword == "const" || keyword == "var" {
			pending[depth] = true
		}

		b.WriteString(strings.Repeat(unit, lineDepth))
		b.WriteString(content)

		if closed := pendingAtOrBelow(pending, newDepth); closed >= 0 {
			last := content[len(content)-1]
			open := last == '{' || last == '(' || last == '['
			if !open && last != ';' && last != ',' && last != '=' {
				b.WriteByte(';')
			}
			if !open && last != '=' {
				delete(pending, closed)
			}
		}

		depth = newDepth
		if depth < 0 {
			depth = 0
		}
	}
	return b.String()
}

// pendingAtOrBelow returns the shallowest depth in `pending` that is at or above `depth` (i.e. whose
// declaration has just closed), or -1 when none closed.
func pendingAtOrBelow(pending map[int]bool, depth int) int {
	closed := -1
	for p := range pending {
		if depth <= p && (closed == -1 || p < closed) {
			closed = p
		}
	}
	return closed
}

// zigDeclarationKeyword returns the declaration keyword of a line, skipping leading modifiers
// (`pub`, `export`, `extern`, `inline`). Returns "" when the line is not a declaration.
func zigDeclarationKeyword(content string) string {
	for _, field := range strings.Fields(content) {
		switch field {
		case "pub", "export", "extern", "inline":
			continue
		}
		return field
	}
	return ""
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
