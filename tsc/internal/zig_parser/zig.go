package zig_parser

import (
	"strconv"
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/zig_scanner"
)

// ParseSourceFile parses a Zig source file. Zig has no direct representation in
// the TypeScript AST, so the source is first lowered to TypeScript (see
// TranslateZig) and then parsed with the regular parser. Zig containers
// (`struct`, `union`, `opaque`) are lowered to TC39 module expressions, which
// the emitter turns into inline module namespaces.
func ParseSourceFile(opts ast.SourceFileParseOptions, sourceText string, scriptKind core.ScriptKind) *ast.SourceFile {
	translated := TranslateZig(sourceText)
	return parseTypeScriptSourceFile(opts, translated, core.ScriptKindTS)
}

// TranslateZig lowers Zig source text to TypeScript source text.
func TranslateZig(sourceText string) string {
	t := &translator{
		toks: zig_scanner.NewLexer(sourceText).Tokens(),
		b:    &strings.Builder{},
	}
	t.translate()
	return t.b.String()
}

type translator struct {
	toks      []zig_scanner.Token
	i         int
	b         *strings.Builder
	testIndex int
}

func (t *translator) write(s string) {
	t.b.WriteString(s)
}

// capture runs f with a temporary output buffer and returns what it wrote.
func (t *translator) capture(f func()) string {
	saved := t.b
	var sb strings.Builder
	t.b = &sb
	f()
	t.b = saved
	return sb.String()
}

func (t *translator) tok() zig_scanner.Token {
	return t.toks[t.i]
}

func (t *translator) peek(offset int) zig_scanner.Token {
	i := t.i + offset
	if i < 0 {
		i = 0
	}
	if i >= len(t.toks) {
		i = len(t.toks) - 1
	}
	return t.toks[i]
}

func (t *translator) atEnd() bool {
	return t.tok().Kind == zig_scanner.EOF
}

func (t *translator) next() zig_scanner.Token {
	tok := t.toks[t.i]
	if t.i < len(t.toks)-1 {
		t.i++
	}
	return tok
}

func (t *translator) isKeyword(keyword string) bool {
	return t.tok().IsKeyword(keyword)
}

func (t *translator) isPunct(punct string) bool {
	return t.tok().IsPunct(punct)
}

func (t *translator) eatKeyword(keyword string) bool {
	if t.isKeyword(keyword) {
		t.next()
		return true
	}
	return false
}

func (t *translator) eatPunct(punct string) bool {
	if t.isPunct(punct) {
		t.next()
		return true
	}
	return false
}

func (t *translator) expectPunct(punct string) bool {
	return t.eatPunct(punct)
}

// translate lowers the whole token stream.
func (t *translator) translate() {
	for !t.atEnd() {
		if t.eatPunct(";") {
			continue
		}
		before := t.i
		t.emitDeclaration(true)
		if t.i == before {
			t.next()
		}
	}
}

// emitDeclaration emits a single top-level or container declaration.
func (t *translator) emitDeclaration(moduleContext bool) {
	exported := false
	extern := false
	for {
		switch {
		case t.isKeyword("pub"):
			t.next()
			exported = true
			continue
		case t.isKeyword("export"):
			t.next()
			exported = true
			continue
		case t.isKeyword("extern"):
			t.next()
			extern = true
			if t.tok().Kind == zig_scanner.String {
				t.next()
			}
			continue
		case t.isKeyword("inline"), t.isKeyword("noinline"), t.isKeyword("threadlocal"):
			t.next()
			continue
		case t.isKeyword("linksection"), t.isKeyword("align"), t.isKeyword("callconv"):
			t.next()
			t.skipBalanced()
			continue
		}
		break
	}

	switch {
	case t.isKeyword("fn"):
		t.emitFunction(exported, extern)
	case t.isKeyword("const"):
		t.emitVariable(exported, true, moduleContext)
	case t.isKeyword("var"):
		t.emitVariable(exported, false, moduleContext)
	case t.isKeyword("test"):
		t.emitTest()
	case t.isKeyword("comptime"):
		t.next()
		if t.isPunct("{") {
			t.emitBlock()
		}
		t.eatPunct(";")
	case t.isKeyword("usingnamespace"):
		t.skipToSemicolon()
	default:
		t.skipEntry()
	}
}

// emitFunction lowers `fn name(params) ReturnType { ... }`.
func (t *translator) emitFunction(exported, extern bool) {
	t.next() // fn
	name := t.identifierText()

	var typeParams []string
	var params []string
	if t.eatPunct("(") {
		index := 0
		for !t.atEnd() && !t.isPunct(")") {
			if t.eatPunct(",") {
				continue
			}
			before := t.i
			param, typeParam := t.parseParam(index)
			if typeParam != "" {
				typeParams = append(typeParams, typeParam)
			} else if param != "" {
				params = append(params, param)
				index++
			}
			if t.i == before {
				t.next()
			}
		}
		t.expectPunct(")")
	}

	t.skipPostParamModifiers()

	ret := ""
	if !t.isPunct("{") && !t.isPunct(";") && !t.isPunct(",") && !t.atEnd() {
		ret = t.capture(func() { t.emitType() })
		t.skipPostParamModifiers()
	}

	if exported {
		t.write("export ")
	}
	if extern {
		t.write("declare ")
	}
	t.write("function ")
	t.write(name)
	if len(typeParams) > 0 {
		t.write("<" + strings.Join(typeParams, ", ") + ">")
	}
	t.write("(" + strings.Join(params, ", ") + ")")
	if ret != "" {
		t.write(": " + ret)
	} else {
		t.write(": void")
	}

	if t.isPunct("{") {
		t.write(" ")
		t.emitBlock()
		t.write("\n")
	} else {
		t.write(";\n")
		t.eatPunct(";")
	}
}

// parseParam parses a single function parameter. It returns the lowered
// parameter text and, when the parameter is a `comptime` type parameter, the
// name of the TypeScript type parameter to declare instead.
func (t *translator) parseParam(index int) (param string, typeParam string) {
	isComptime := false
	for {
		if t.isKeyword("comptime") {
			t.next()
			isComptime = true
			continue
		}
		if t.isKeyword("noalias") {
			t.next()
			continue
		}
		break
	}
	if t.atEnd() {
		return "", ""
	}
	if t.isPunct("...") {
		t.next()
		return "...args: any[]", ""
	}

	name := ""
	if t.tok().Kind == zig_scanner.Identifier && t.peek(1).IsPunct(":") {
		name = t.next().Value
		if name == "_" {
			name = ""
		}
		t.next() // ':'
	}

	typ := "any"
	if !t.isPunct(",") && !t.isPunct(")") && !t.atEnd() {
		typ = t.capture(func() { t.emitType() })
	}
	t.skipPostParamModifiers()

	if typ == "type" || isComptime && typ == "type" {
		if name != "" {
			return "", tsSafeIdent(name)
		}
		return "", "T"
	}
	if name == "" {
		name = "arg" + strconv.Itoa(index)
	}
	return tsSafeIdent(name) + ": " + typ, ""
}

func (t *translator) skipPostParamModifiers() {
	for t.isKeyword("callconv") || t.isKeyword("align") || t.isKeyword("linksection") || t.isKeyword("addrspace") {
		t.next()
		t.skipBalanced()
	}
}

// emitVariable lowers a `const`/`var` declaration.
func (t *translator) emitVariable(exported, isConst, moduleContext bool) {
	t.next() // const / var
	name := t.identifierText()

	typ := ""
	if t.eatPunct(":") {
		typ = t.capture(func() { t.emitType() })
	}

	if t.isPunct("=") && isContainerStartToken(t.peek(1)) {
		t.next() // '='
		t.emitContainerValue(exported, name, moduleContext)
		return
	}
	if t.isPunct("=") && t.peek(1).Kind == zig_scanner.Builtin && t.peek(1).Value == "import" {
		saved := t.i
		t.next() // '='
		spec, external := normalizeImportSpecifier(t.parseImportSpecifier())
		if t.isPunct(";") || t.isPunct("}") || t.atEnd() {
			if external {
				t.write("import * as " + name + " from " + quoteJS(spec) + ";\n")
				if exported {
					t.write("export { " + name + " };\n")
				}
			} else {
				// `@import("builtin")` / `@import("root")` do not map to real modules.
				if exported {
					t.write("export ")
				}
				t.write("const " + name + ": any = (globalThis as any);\n")
			}
			t.eatPunct(";")
			return
		}
		// `@import(...)` is only an import declaration when it is the whole
		// initializer (e.g. `@import("foo").Bar` is an expression).
		t.i = saved
	}

	// `const Name = fn(...) T;` and similar declare a type alias in Zig.
	if t.isPunct("=") && startsType(t.peek(1)) {
		t.next() // '='
		typ := t.capture(func() { t.emitType() })
		if exported {
			t.write("export ")
		}
		t.write("type " + name + " = " + typ + ";\n")
		t.eatPunct(";")
		return
	}

	keyword := "let"
	if isConst {
		keyword = "const"
	}
	if exported {
		t.write("export ")
	}
	t.write(keyword + " " + name)
	if typ != "" {
		t.write(": " + typ)
	}
	if t.eatPunct("=") {
		t.write(" = ")
		t.emitExpr(0)
	} else if typ != "" {
		// Uninitialized declaration; `const` requires an initializer in TS.
		if keyword == "const" {
			t.write(" = undefined as unknown as " + typ)
		}
	} else {
		t.write(" = undefined")
	}
	t.write(";\n")
	t.eatPunct(";")
}

// normalizeImportSpecifier converts a Zig `@import` specifier into a TypeScript
// module specifier. `@import("builtin")` and `@import("root")` have no module
// to resolve to and are reported as non-external. Zig resolves `@import("x.zig")`
// relative to the current file, so a leading `./` is added for such paths.
func normalizeImportSpecifier(spec string) (string, bool) {
	if spec == "builtin" || spec == "root" {
		return "", false
	}
	if strings.HasSuffix(spec, ".zig") && !strings.HasPrefix(spec, ".") && !strings.HasPrefix(spec, "/") {
		return "./" + spec, true
	}
	return spec, true
}

func isContainerStartToken(tok zig_scanner.Token) bool {
	if tok.Kind != zig_scanner.Keyword {
		return false
	}
	switch tok.Value {
	case "struct", "union", "opaque", "enum", "packed":
		return true
	}
	return false
}

// startsType reports whether tok clearly begins a Zig type expression. It is
// used to distinguish `const T = SomeType;` type aliases from value bindings
// without needing full type inference.
func startsType(tok zig_scanner.Token) bool {
	switch tok.Kind {
	case zig_scanner.Keyword:
		switch tok.Value {
		case "fn", "error", "anyerror":
			return true
		}
	case zig_scanner.Punct:
		switch tok.Text {
		case "?", "[":
			return true
		}
	}
	return false
}

// emitContainerValue lowers a container literal used as an initializer.
func (t *translator) emitContainerValue(exported bool, name string, moduleContext bool) {
	if t.isKeyword("packed") {
		t.next()
	}
	kind := t.tok().Value
	t.next() // struct / union / opaque / enum

	if kind == "enum" {
		t.emitEnumBody(name, exported)
		t.eatPunct(";")
		return
	}

	t.skipContainerTag()

	// Emit the container's shape as an explicit type alias. Doing this before
	// the value declaration (and rewinding the token stream) keeps recursive
	// struct references working without a circular `typeof` self-reference.
	bodyStart := t.i
	typeText := t.capture(func() { t.emitObjectTypeLiteral() })
	t.i = bodyStart

	if exported {
		t.write("export ")
	}
	t.write("type " + name + " = " + typeText + ";\n")

	if moduleContext {
		if exported {
			t.write("export ")
		}
		t.write("const " + name + " = module ")
		t.emitModuleBody()
		t.write(";\n")
		return
	}

	// Inside a function body we cannot await an inline module, so lower to an
	// object literal typed by the struct's shape.
	if exported {
		t.write("export ")
	}
	t.write("const " + name + " = {} as " + typeText + ";\n")
	// Consume the container body that was only re-read to build the type.
	if t.isPunct("{") {
		t.skipBalanced()
	}
}

// skipContainerTag skips an optional `(enum)` tag on a union declaration.
func (t *translator) skipContainerTag() {
	if t.isPunct("(") {
		t.skipBalanced()
	}
}

// emitModuleBody lowers the members of a container to a module block.
func (t *translator) emitModuleBody() {
	if !t.expectPunct("{") {
		t.write("{}")
		return
	}
	t.write("{")
	for !t.atEnd() && !t.isPunct("}") {
		if t.eatPunct(",") || t.eatPunct(";") {
			continue
		}
		before := t.i
		t.emitContainerMember()
		if t.i == before {
			t.next()
		}
	}
	t.expectPunct("}")
	t.write("}")
}

// emitContainerMember lowers one member of a `struct`/`union`/`opaque` body.
func (t *translator) emitContainerMember() {
	exported := false
	for t.isKeyword("pub") {
		t.next()
		exported = true
	}
	switch {
	case t.isKeyword("fn"):
		t.emitFunction(exported, false)
	case t.isKeyword("const"):
		t.emitVariable(exported, true, true)
	case t.isKeyword("var"):
		t.emitVariable(exported, false, true)
	case t.isKeyword("test"):
		t.emitTest()
	case t.isKeyword("comptime"):
		t.next()
		if t.isPunct("{") {
			t.emitBlock()
		}
		t.eatPunct(";")
		return
	case t.isKeyword("usingnamespace"):
		t.skipToSemicolon()
		return
	}

	// Field declaration: `name: Type` with an optional default value.
	if t.tok().Kind == zig_scanner.Identifier && t.peek(1).IsPunct(":") {
		name := t.next().Value
		t.next() // ':'
		typ := t.capture(func() { t.emitType() })
		t.skipPostParamModifiers()
		def := ""
		if t.eatPunct("=") {
			def = t.capture(func() { t.emitExpr(0) })
		}
		// Struct fields are public in Zig, so they are always exported from the
		// lowered module.
		t.write("export let " + tsSafeIdent(name) + ": " + typ)
		if def != "" {
			t.write(" = " + def)
		} else {
			// Give the field a value so it is part of the module namespace type.
			t.write(" = undefined as unknown as " + typ)
		}
		t.write(";\n")
		t.eatPunct(",")
		t.eatPunct(";")
		return
	}
	t.skipEntry()
}

// emitEnumBody lowers an enum to a module of string constants plus a union type.
// This mirrors how Zig enum literals (`.north`) are lowered to string literals,
// and keeps the enum usable both as a value namespace and as a type.
func (t *translator) emitEnumBody(name string, exported bool) {
	t.skipContainerTag()
	members := t.parseEnumMembers()
	if exported {
		t.write("export ")
	}
	t.write("const " + name + " = module {")
	for i, member := range members {
		if i > 0 {
			t.write(" ")
		}
		t.write("export const " + tsSafeMember(member) + " = " + quoteJS(member) + ";")
	}
	t.write("};\n")
	if exported {
		t.write("export ")
	}
	t.write("type " + name + " = " + enumUnionType(members) + ";\n")
}

// parseEnumMembers consumes an enum member list and returns the member names.
func (t *translator) parseEnumMembers() []string {
	var members []string
	if !t.expectPunct("{") {
		return members
	}
	for !t.atEnd() && !t.isPunct("}") {
		if t.eatPunct(",") {
			continue
		}
		if t.isKeyword("fn") || t.isKeyword("const") || t.isKeyword("var") || t.isKeyword("pub") || t.isKeyword("comptime") || t.isKeyword("usingnamespace") || t.isKeyword("test") {
			t.skipEntry()
			continue
		}
		if t.tok().Kind != zig_scanner.Identifier {
			t.next()
			continue
		}
		member := t.next().Value
		if t.eatPunct("=") {
			t.skipToCommaOrCloseBrace()
		}
		if member == "_" {
			continue
		}
		members = append(members, member)
	}
	t.expectPunct("}")
	return members
}

func enumUnionType(members []string) string {
	if len(members) == 0 {
		return "never"
	}
	parts := make([]string, 0, len(members))
	for _, member := range members {
		parts = append(parts, quoteJS(member))
	}
	return strings.Join(parts, " | ")
}

// emitTest lowers `test "name" { ... }` to a function declaration.
func (t *translator) emitTest() {
	t.next() // test
	name := ""
	if t.tok().Kind == zig_scanner.String {
		name = t.next().Value
	}
	if name == "" {
		t.testIndex++
		name = strconv.Itoa(t.testIndex)
	}
	t.write("export function test_" + sanitizeName(name) + "(): void ")
	if t.isPunct("{") {
		t.emitBlock()
	}
	t.write("\n")
	t.eatPunct(";")
}

// --- Statements ---

func (t *translator) emitStatement() {
	if t.tok().Kind == zig_scanner.Identifier && t.tok().Value == "_" && t.peek(1).IsPunct("=") {
		// `_ = expr;` discards a value; evaluate it for side effects only.
		t.next()
		t.next()
		t.write("void (")
		t.emitExpr(0)
		t.write(");")
		t.eatPunct(";")
		return
	}
	switch {
	case t.isKeyword("const"), t.isKeyword("var"):
		t.emitVariable(false, t.isKeyword("const"), false)
	case t.isKeyword("if"):
		t.emitIfStatement()
	case t.isKeyword("while"):
		t.emitWhileStatement()
	case t.isKeyword("for"):
		t.emitForStatement()
	case t.isKeyword("switch"):
		t.write(t.capture(func() { t.emitSwitch(false) }))
	case t.isKeyword("return"):
		t.next()
		t.write("return")
		if !t.isPunct(";") && !t.isPunct("}") && !t.atEnd() {
			t.write(" ")
			t.emitExpr(0)
		}
		t.write(";")
		t.eatPunct(";")
	case t.isKeyword("break"):
		t.next()
		t.write("break;")
		t.skipToSemicolon()
	case t.isKeyword("continue"):
		t.next()
		t.write("continue;")
		t.skipToSemicolon()
	case t.isKeyword("defer"), t.isKeyword("errdefer"):
		t.next()
		t.skipToSemicolon()
	case t.isKeyword("unreachable"):
		t.next()
		t.write(`throw new Error("unreachable");`)
		t.eatPunct(";")
	case t.isKeyword("comptime"):
		t.next()
		if t.isPunct("{") {
			t.emitBlock()
		}
		t.eatPunct(";")
	case t.isPunct("{"):
		t.emitBlock()
	case t.tok().Kind == zig_scanner.Identifier && t.peek(1).IsPunct(":") && t.peek(2).IsPunct("{"):
		// Labeled block: `label: { ... }`
		t.next()
		t.next()
		t.emitBlock()
		t.eatPunct(";")
	default:
		t.emitExpr(0)
		t.write(";")
		t.eatPunct(";")
	}
}

func (t *translator) emitBlock() {
	if !t.expectPunct("{") {
		return
	}
	t.write("{")
	for !t.atEnd() && !t.isPunct("}") {
		if t.eatPunct(";") {
			continue
		}
		before := t.i
		t.emitStatement()
		if t.i == before {
			t.next()
		}
	}
	t.expectPunct("}")
	t.write("}")
}

func (t *translator) emitStatementOrBlock() {
	if t.isPunct("{") {
		t.emitBlock()
	} else {
		t.emitStatement()
	}
}

// parseCondition parses `if`/`while` condition plus its optional `|capture|`.
func (t *translator) parseCondition() (cond string, capture string) {
	if t.eatPunct("(") {
		cond = t.capture(func() { t.emitExpr(0) })
		t.expectPunct(")")
	} else {
		cond = t.capture(func() { t.emitExpr(0) })
	}
	if t.isPunct("|") {
		t.next()
		if t.tok().Kind == zig_scanner.Identifier {
			capture = tsSafeIdent(t.next().Value)
		}
		for !t.atEnd() && !t.isPunct("|") {
			t.next()
		}
		t.eatPunct("|")
	}
	return cond, capture
}

// skipCapture skips an optional `|a, b|` payload capture.
func (t *translator) skipCapture() {
	if !t.isPunct("|") {
		return
	}
	t.next()
	for !t.atEnd() && !t.isPunct("|") {
		t.next()
	}
	t.eatPunct("|")
}

func (t *translator) emitIfStatement() {
	t.next() // if
	cond, capture := t.parseCondition()
	if capture != "" {
		// Optional payload capture: `if (opt) |v| ...` binds `v` to the value.
		t.write("if (" + cond + " != null) { const " + capture + " = " + cond + "; ")
		t.emitStatementOrBlock()
		t.write(" }")
	} else {
		t.write("if (" + cond + ") ")
		t.emitStatementOrBlock()
	}
	if t.eatKeyword("else") {
		t.write(" else ")
		if t.isKeyword("if") {
			t.emitIfStatement()
		} else {
			t.emitStatementOrBlock()
		}
	}
}

func (t *translator) emitWhileStatement() {
	t.next() // while
	cond, capture := t.parseCondition()
	// Optional `: (continueExpr)` is dropped.
	if t.eatPunct(":") {
		t.skipBalanced()
	}
	body := func() {
		if capture != "" {
			t.write("{ const " + capture + " = " + cond + "; ")
			t.emitStatementOrBlock()
			t.write(" }")
		} else {
			t.emitStatementOrBlock()
		}
	}
	if t.eatKeyword("else") {
		// Zig's while-else runs on normal completion; approximate by running the
		// else block after the loop.
		condExpr := cond
		if capture != "" {
			condExpr = cond + " != null"
		}
		t.write("while (" + condExpr + ") ")
		body()
		t.write(" ")
		t.emitStatementOrBlock()
		return
	}
	condExpr := cond
	if capture != "" {
		condExpr = cond + " != null"
	}
	t.write("while (" + condExpr + ") ")
	body()
}

func (t *translator) emitForStatement() {
	t.next() // for
	var iter string
	if t.eatPunct("(") {
		start := t.capture(func() { t.emitExpr(0) })
		if t.eatPunct("..") || t.eatPunct("...") {
			end := t.capture(func() { t.emitExpr(0) })
			iter = "Array.from({ length: (" + end + ") - (" + start + ") }, (_, __i) => (" + start + ") + __i)"
		} else {
			iter = start
		}
		t.expectPunct(")")
	} else {
		start := t.capture(func() { t.emitExpr(0) })
		if t.eatPunct("..") || t.eatPunct("...") {
			end := t.capture(func() { t.emitExpr(0) })
			iter = "Array.from({ length: (" + end + ") - (" + start + ") }, (_, __i) => (" + start + ") + __i)"
		} else {
			iter = start
		}
	}

	item, index := "", ""
	if t.isPunct("|") {
		t.next()
		var names []string
		for !t.atEnd() && !t.isPunct("|") {
			if t.eatPunct(",") {
				continue
			}
			if t.tok().Kind == zig_scanner.Identifier {
				name := t.next().Value
				if name != "_" {
					names = append(names, tsSafeIdent(name))
				}
			} else {
				t.next()
			}
		}
		t.eatPunct("|")
		if len(names) > 0 {
			item = names[0]
		}
		if len(names) > 1 {
			index = names[1]
		}
	}

	switch {
	case index != "" && item != "":
		t.write("for (const [" + index + ", " + item + "] of (" + iter + ").entries()) ")
	case item != "":
		t.write("for (const " + item + " of " + iter + ") ")
	default:
		t.write("for (const _ of " + iter + ") ")
	}
	t.emitStatementOrBlock()
}

// emitSwitch lowers a switch statement (`asExpression == false`) or expression.
func (t *translator) emitSwitch(asExpression bool) {
	t.next() // switch
	var subject string
	if t.eatPunct("(") {
		subject = t.capture(func() { t.emitExpr(0) })
		t.expectPunct(")")
	} else {
		subject = t.capture(func() { t.emitExpr(0) })
	}

	// The subject is cast to `any` so that switching on a lowered Zig value
	// (e.g. a struct/union or an enum string) is always comparable.
	if asExpression {
		t.write("(() => { switch ((" + subject + ") as any) {")
	} else {
		t.write("switch ((" + subject + ") as any) {")
	}
	hasDefault := false
	if t.expectPunct("{") {
		for !t.atEnd() && !t.isPunct("}") {
			if t.eatPunct(",") {
				continue
			}
			before := t.i
			if t.emitSwitchProng(subject, asExpression) {
				hasDefault = true
			}
			if t.i == before {
				t.next()
			}
		}
		t.expectPunct("}")
	}
	if asExpression && !hasDefault {
		// Guarantee the IIFE has a return on every path.
		t.write(" default: return undefined as never;")
	}
	t.write("}")
	if asExpression {
		t.write("})()")
	}
}

// emitSwitchProng lowers a single switch prong and reports whether it was the
// `else`/default prong.
func (t *translator) emitSwitchProng(subject string, asExpression bool) bool {
	if t.eatKeyword("else") {
		t.expectPunct("=>")
		t.write(" default:")
		t.emitSwitchProngBody(asExpression, subject, "", "")
		return true
	}
	// Collect one or more case values until `=>`.
	firstValue := ""
	for {
		if t.isPunct("=>") {
			t.next()
			break
		}
		if t.atEnd() || t.isPunct("}") {
			return false
		}
		if t.eatPunct(",") {
			continue
		}
		var value string
		if t.isPunct(".") {
			t.next()
			value = quoteJS(t.identifierText())
		} else {
			value = t.capture(func() { t.emitExpr(0) })
		}
		if firstValue == "" {
			firstValue = value
		}
		t.write(" case " + value + ":")
	}
	// Optional payload capture, e.g. `.circle => |r| ...`.
	capture := ""
	if t.isPunct("|") {
		t.next()
		if t.tok().Kind == zig_scanner.Identifier {
			capture = tsSafeIdent(t.next().Value)
		}
		for !t.atEnd() && !t.isPunct("|") {
			t.next()
		}
		t.eatPunct("|")
	}
	t.emitSwitchProngBody(asExpression, subject, firstValue, capture)
	return false
}

func (t *translator) emitSwitchProngBody(asExpression bool, subject, firstValue, capture string) {
	binding := ""
	if capture != "" {
		if firstValue != "" {
			binding = "const " + capture + " = (" + subject + ")[" + firstValue + "] as any; "
		} else {
			binding = "const " + capture + " = undefined as any; "
		}
	}
	if t.isPunct("{") {
		block := t.capture(func() { t.emitBlock() })
		if asExpression {
			t.write(" return (() => { " + binding + block + " })();")
		} else {
			t.write(" { " + binding + block + " }")
		}
	} else if asExpression {
		t.write(" return (() => { " + binding + "return ")
		t.emitExpr(0)
		t.write("; })();")
	} else if binding != "" {
		t.write(" { " + binding)
		t.emitExpr(0)
		t.write("; }")
	} else {
		t.write(" ")
		t.emitExpr(0)
		t.write(";")
	}
	if !asExpression {
		t.write(" break;")
	}
}

// --- Types ---

func (t *translator) emitType() {
	base := t.capture(func() { t.emitTypeBase() })
	if t.isPunct("!") {
		// Error union type `E!T`: the error set has no TypeScript equivalent, so
		// only the payload type is kept.
		t.next()
		t.emitType()
		return
	}
	t.write(base)
}

func (t *translator) emitTypeBase() {
	switch {
	case t.isPunct("?"):
		t.next()
		inner := t.capture(func() { t.emitType() })
		t.write("(" + inner + " | null)")
	case t.isPunct("!"):
		t.next()
		t.emitType()
	case t.isKeyword("const") || t.isKeyword("volatile") || t.isKeyword("allowzero"):
		t.next()
		t.emitType()
	case t.isKeyword("anyerror"):
		t.next()
		if t.eatPunct("!") {
			t.emitType()
		} else {
			t.write("Error")
		}
	case t.isKeyword("error"):
		t.next()
		if t.isPunct("{") {
			t.skipBalanced()
		}
		if t.eatPunct("!") {
			t.emitType()
		} else {
			t.write("Error")
		}
	case t.isPunct("*"):
		t.next()
		t.emitType()
	case t.isPunct("["):
		t.emitArrayOrSliceType()
	case t.isKeyword("fn"):
		t.emitFunctionType()
	case t.isKeyword("struct"):
		t.next()
		t.emitObjectTypeLiteral()
	case t.isKeyword("union"):
		t.next()
		t.skipContainerTag()
		t.emitObjectTypeLiteral()
	case t.isKeyword("enum"):
		t.next()
		t.skipContainerTag()
		members := t.parseEnumMembers()
		t.write(enumUnionType(members))
	case t.isKeyword("opaque"):
		t.next()
		t.write("unknown")
	case t.isKeyword("anytype"):
		t.next()
		t.write("any")
	case t.isKeyword("anyframe"), t.isKeyword("type"):
		t.next()
		t.write("unknown")
	case t.isKeyword("void"):
		t.next()
		t.write("void")
	case t.isKeyword("noreturn"):
		t.next()
		t.write("never")
	case t.isKeyword("bool"):
		t.next()
		t.write("boolean")
	case t.tok().Kind == zig_scanner.Identifier && numericTypes[t.tok().Value]:
		t.next()
		t.write("number")
	case t.tok().Kind == zig_scanner.Builtin && t.tok().Value == "TypeOf":
		t.next()
		if t.eatPunct("(") {
			expr := t.capture(func() { t.emitExpr(0) })
			t.expectPunct(")")
			if isSimpleTypeName(expr) {
				t.write("typeof " + expr)
			} else {
				t.write("unknown")
			}
		} else {
			t.write("unknown")
		}
	case t.tok().Kind == zig_scanner.Builtin:
		// Builtins used in type position (e.g. `@EnumLiteral()`, `@Type(...)`).
		// They have no TypeScript equivalent, so lower to a usable type instead
		// of emitting a value expression that would fail to parse.
		name := t.next().Value
		if t.isPunct("(") {
			t.skipBalanced()
		}
		switch name {
		case "EnumLiteral":
			t.write("string")
		default:
			t.write("unknown")
		}
	case t.isPunct("("):
		t.next()
		inner := t.capture(func() { t.emitType() })
		t.expectPunct(")")
		t.write(inner)
	default:
		if t.tok().Kind == zig_scanner.Identifier {
			if builtin, ok := builtinTypeName(t.tok().Value); ok {
				t.next()
				t.write(builtin)
				return
			}
		}
		t.emitQualifiedName()
	}
}

func (t *translator) emitArrayOrSliceType() {
	t.next() // [
	isSlice := t.isPunct("]")
	sentinel := false
	if t.isPunct("*") {
		t.next()
	}
	if t.isPunct(":") {
		sentinel = true
		t.next()
		if !t.isPunct("]") {
			t.capture(func() { t.emitExpr(0) })
		}
	} else if !isSlice && !t.isPunct("]") {
		t.capture(func() { t.emitExpr(0) })
		// `[N:sentinel]T` carries a sentinel value after the length.
		if t.isPunct(":") {
			sentinel = true
			t.next()
			if !t.isPunct("]") {
				t.capture(func() { t.emitExpr(0) })
			}
		}
	}
	t.expectPunct("]")
	for t.isKeyword("const") {
		t.next()
	}
	if (isSlice || sentinel) && t.tok().Kind == zig_scanner.Identifier && t.tok().Value == "u8" {
		t.next()
		t.write("string")
		return
	}
	elem := t.capture(func() { t.emitType() })
	t.write(elem + "[]")
}

func (t *translator) emitFunctionType() {
	t.next() // fn
	var params []string
	if t.eatPunct("(") {
		index := 0
		for !t.atEnd() && !t.isPunct(")") {
			if t.eatPunct(",") {
				continue
			}
			before := t.i
			param, _ := t.parseParam(index)
			if param != "" {
				params = append(params, param)
				index++
			}
			if t.i == before {
				t.next()
			}
		}
		t.expectPunct(")")
	}
	t.skipPostParamModifiers()
	t.write("(" + strings.Join(params, ", ") + ") => ")
	if t.atEnd() || t.isPunct(";") || t.isPunct(",") || t.isPunct(")") || t.isPunct("}") {
		t.write("void")
		return
	}
	t.emitType()
}

// emitObjectTypeLiteral lowers a `struct { ... }`/`union { ... }` type to an
// anonymous TypeScript object type.
func (t *translator) emitObjectTypeLiteral() {
	if !t.expectPunct("{") {
		t.write("{}")
		return
	}
	t.write("{ ")
	for !t.atEnd() && !t.isPunct("}") {
		if t.eatPunct(",") || t.eatPunct(";") {
			continue
		}
		if t.isKeyword("pub") || t.isKeyword("fn") || t.isKeyword("const") || t.isKeyword("var") || t.isKeyword("comptime") || t.isKeyword("usingnamespace") || t.isKeyword("test") {
			t.skipEntry()
			continue
		}
		if t.tok().Kind == zig_scanner.Identifier && t.peek(1).IsPunct(":") {
			name := t.next().Value
			t.next() // ':'
			typ := t.capture(func() { t.emitType() })
			t.write(tsSafeMember(name) + ": " + typ + "; ")
			if t.eatPunct("=") {
				t.skipToCommaOrCloseBrace()
			}
			continue
		}
		before := t.i
		t.skipEntry()
		if t.i == before {
			t.next()
		}
	}
	t.expectPunct("}")
	t.write("}")
}

func (t *translator) emitQualifiedName() {
	first := true
	for {
		switch t.tok().Kind {
		case zig_scanner.Identifier, zig_scanner.Keyword:
			name := t.next().Value
			if name == "len" {
				name = "length"
			}
			if first {
				t.write(tsSafeIdent(name))
				first = false
			} else {
				t.write("." + tsSafeMember(name))
			}
		case zig_scanner.Builtin:
			t.write(t.capture(func() { t.parseBuiltin() }))
			first = false
		default:
			if first {
				t.next()
				t.write("unknown")
			}
			return
		}
		if !t.eatPunct(".") {
			break
		}
	}
	// A generic instantiation in type position, e.g. `AutoHashMap(K, V)`, has no
	// TypeScript equivalent; drop the type arguments.
	if t.isPunct("(") {
		t.skipBalanced()
	}
}

// --- Expressions ---

type binOp struct {
	prec       int
	op         string
	rightAssoc bool
}

var binaryOps = map[string]binOp{
	"=":   {1, "=", true},
	"+=":  {1, "+=", true},
	"-=":  {1, "-=", true},
	"*=":  {1, "*=", true},
	"/=":  {1, "/=", true},
	"%=":  {1, "%=", true},
	"&=":  {1, "&=", true},
	"|=":  {1, "|=", true},
	"^=":  {1, "^=", true},
	"<<=": {1, "<<=", true},
	">>=": {1, ">>=", true},

	"orelse": {2, "??", false},
	"catch":  {2, "??", false},
	"or":     {3, "||", false},
	"and":    {4, "&&", false},

	"==": {5, "===", false},
	"!=": {5, "!==", false},
	"<":  {5, "<", false},
	">":  {5, ">", false},
	"<=": {5, "<=", false},
	">=": {5, ">=", false},

	"|": {6, "|", false},
	"^": {7, "^", false},
	"&": {8, "&", false},

	"<<": {9, "<<", false},
	">>": {9, ">>", false},

	"+":  {10, "+", false},
	"-":  {10, "-", false},
	"++": {10, "+", false},

	"*":  {11, "*", false},
	"/":  {11, "/", false},
	"%":  {11, "%", false},
	"**": {11, "*", false},
}

func (t *translator) emitExpr(minPrec int) {
	left := t.capture(func() { t.emitUnary() })
	for {
		tok := t.tok()
		info, ok := binaryOps[tok.Value]
		if !ok || tok.Kind != zig_scanner.Punct && tok.Kind != zig_scanner.Keyword {
			break
		}
		if info.prec < minPrec {
			break
		}
		t.next()
		if tok.IsKeyword("orelse") || tok.IsKeyword("catch") {
			if t.isPunct("|") {
				t.skipCapture()
			}
			// `x orelse return y` / `x catch return y` return from the enclosing
			// function on the error path. TypeScript has no direct equivalent, so
			// lower the early return away and keep the fallback value.
			if t.isKeyword("return") {
				t.next()
			} else if t.isKeyword("break") || t.isKeyword("continue") {
				t.next()
				left = "(" + left + " " + info.op + " undefined)"
				continue
			}
		}
		nextMin := info.prec + 1
		if info.rightAssoc {
			nextMin = info.prec
		}
		right := t.capture(func() { t.emitExpr(nextMin) })
		left = "(" + left + " " + info.op + " " + right + ")"
		if tok.IsKeyword("orelse") {
			// `x orelse y` is non-optional in Zig.
			left += "!"
		}
	}
	t.write(left)
}

func (t *translator) emitUnary() {
	tok := t.tok()
	switch {
	case tok.IsKeyword("try"), tok.IsKeyword("comptime"):
		t.next()
		t.emitUnary()
	case tok.IsKeyword("async"):
		t.next()
		t.emitUnary()
	case tok.IsKeyword("await"):
		t.next()
		t.write("await ")
		t.emitUnary()
	case tok.IsKeyword("unreachable"):
		t.next()
		t.write(`(() => { throw new Error("unreachable"); })()`)
	case tok.IsKeyword("resume"):
		t.next()
		t.write("undefined")
	case tok.IsPunct("!"):
		t.next()
		t.write("!")
		t.emitUnary()
	case tok.IsPunct("-"):
		t.next()
		t.write("-")
		t.emitUnary()
	case tok.IsPunct("~"):
		t.next()
		t.write("~")
		t.emitUnary()
	case tok.IsPunct("&"), tok.IsPunct("*"):
		// address-of/dereference have no TypeScript equivalent; erase them.
		t.next()
		t.emitUnary()
	default:
		t.emitPostfix()
	}
}

func (t *translator) emitPostfix() {
	expr := t.capture(func() { t.emitPrimary() })
loop:
	for {
		switch {
		case t.isPunct("("):
			t.next()
			var args []string
			for !t.atEnd() && !t.isPunct(")") {
				if t.eatPunct(",") {
					continue
				}
				before := t.i
				args = append(args, t.capture(func() { t.emitExpr(0) }))
				if t.i == before {
					t.next()
				}
			}
			t.expectPunct(")")
			expr = expr + "(" + strings.Join(args, ", ") + ")"
		case t.isPunct("["):
			t.next()
			var parts []string
			hasRange := false
			for !t.atEnd() && !t.isPunct("]") {
				if t.isPunct("..") || t.isPunct("...") {
					hasRange = true
					t.next()
					continue
				}
				if t.eatPunct(",") {
					continue
				}
				before := t.i
				parts = append(parts, t.capture(func() { t.emitExpr(0) }))
				if t.i == before {
					t.next()
				}
			}
			t.expectPunct("]")
			if hasRange {
				expr = expr + ".slice(" + strings.Join(parts, ", ") + ")"
			} else if len(parts) > 0 {
				expr = expr + "[" + strings.Join(parts, ", ") + "]"
			}
		case t.isPunct("."):
			next := t.peek(1)
			if next.IsPunct("?") {
				t.next()
				t.next()
				expr = expr + "!"
				continue
			}
			if next.IsPunct("*") {
				t.next()
				t.next()
				continue
			}
			if next.Kind == zig_scanner.Identifier || next.Kind == zig_scanner.Keyword {
				t.next()
				name := t.next().Value
				if name == "len" {
					name = "length"
				}
				expr = expr + "." + tsSafeMember(name)
				continue
			}
			break loop
		case t.isPunct("{"):
			// Struct initializer: `Type{ .field = value }`. Only cast when the
			// callee is a plain type name; generic instantiations and builtins
			// (e.g. `@Vector(3, T){ ... }`) lower to expressions that are not
			// valid TypeScript types.
			obj := t.capture(func() { t.emitObjectLiteral() })
			if isSimpleTypeName(expr) {
				expr = "(" + obj + " as " + expr + ")"
			} else {
				expr = obj
			}
		default:
			break loop
		}
	}
	t.write(expr)
}

func (t *translator) emitPrimary() {
	tok := t.tok()
	switch tok.Kind {
	case zig_scanner.Number:
		t.next()
		t.write(normalizeNumber(tok.Value))
	case zig_scanner.String:
		t.emitString()
	case zig_scanner.Char:
		t.next()
		t.write(quoteChar(tok.Value))
	case zig_scanner.Identifier:
		t.next()
		switch tok.Value {
		case "true", "false", "null", "undefined":
			t.write(tok.Value)
		default:
			if _, ok := builtinTypeName(tok.Value); ok {
				// Builtin type names (`u8`, `bool`, `void`, ...) have no value
				// representation; when they appear in expression position (for
				// example as a call argument like `HashMap(void)`) lower them to
				// a placeholder so the generated TypeScript parses.
				t.write("undefined")
			} else {
				t.write(tsSafeIdent(tok.Value))
			}
		}
	case zig_scanner.Builtin:
		t.parseBuiltin()
	case zig_scanner.Keyword:
		switch tok.Value {
		case "if":
			t.emitIfExpression()
		case "switch":
			t.emitSwitch(true)
		case "while", "for":
			t.next()
			t.skipBalancedOrExpr()
			t.write("undefined")
		case "null", "undefined":
			t.next()
			t.write(tok.Value)
		case "error":
			t.next()
			if t.eatPunct(".") {
				name := t.identifierText()
				t.write("(() => { throw new Error(" + quoteJS(name) + "); })()")
			} else {
				t.write(`(() => { throw new Error("error"); })()`)
			}
		case "anytype":
			t.next()
			t.write("any")
		case "type":
			t.next()
			t.write("unknown")
		case "struct", "union", "opaque", "enum", "packed":
			t.emitAnonymousContainer()
		default:
			t.next()
			t.write("undefined")
		}
	default:
		switch {
		case tok.IsPunct("("):
			t.next()
			inner := t.capture(func() { t.emitExpr(0) })
			t.expectPunct(")")
			t.write("(" + inner + ")")
		case tok.IsPunct("."):
			t.next()
			if t.isPunct("{") {
				t.emitObjectLiteral()
			} else {
				// Enum literal, e.g. `.north`; Zig enums lower to string unions.
				t.write(quoteJS(t.identifierText()))
			}
		default:
			t.next()
			t.write("undefined")
		}
	}
}

// normalizeNumber rewrites Zig numeric literals that TypeScript cannot parse.
// In particular Zig hexadecimal floating point literals (`0x1.8p3`) are not
// valid JavaScript and are converted to their decimal representation.
func normalizeNumber(value string) string {
	if len(value) >= 2 && value[0] == '0' && (value[1] == 'x' || value[1] == 'X') && strings.ContainsAny(value, ".pP") {
		hex := value
		if !strings.ContainsAny(hex, "pP") {
			// Go's hexadecimal float parser requires a binary exponent.
			hex += "p0"
		}
		if f, err := strconv.ParseFloat(hex, 64); err == nil {
			return strconv.FormatFloat(f, 'g', -1, 64)
		}
	}
	return value
}

func (t *translator) emitString() {
	if t.tok().IsMultilineString() {
		var lines []string
		for t.tok().Kind == zig_scanner.String && t.tok().IsMultilineString() {
			lines = append(lines, t.next().Value)
		}
		t.write(quoteMultilineJS(lines))
		return
	}
	tok := t.next()
	t.write(quoteJS(tok.Value))
}

func (t *translator) emitIfExpression() {
	t.next() // if
	cond, capture := t.parseCondition()

	then := ""
	if t.isPunct("{") {
		block := t.capture(func() { t.emitBlock() })
		if capture != "" {
			then = "(() => { const " + capture + " = " + cond + "; " + block + " })()"
		} else {
			then = "(() => " + block + ")()"
		}
	} else {
		expr := t.capture(func() { t.emitExpr(0) })
		if capture != "" {
			then = "(() => { const " + capture + " = " + cond + "; return " + expr + "; })()"
		} else {
			then = expr
		}
	}

	elseExpr := "undefined"
	if t.eatKeyword("else") {
		if t.isPunct("{") {
			elseExpr = "(() => " + t.capture(func() { t.emitBlock() }) + ")()"
		} else if t.isKeyword("if") {
			elseExpr = t.capture(func() { t.emitIfExpression() })
		} else {
			elseExpr = t.capture(func() { t.emitExpr(0) })
		}
	}
	condExpr := cond
	if capture != "" {
		condExpr = cond + " != null"
	}
	t.write("(" + condExpr + " ? " + then + " : " + elseExpr + ")")
}

// isSimpleTypeName reports whether name is a plain (possibly qualified)
// TypeScript type reference, i.e. an identifier made up of identifier
// characters and dots.
func isSimpleTypeName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		valid := r == '_' || r == '$' || r == '.' ||
			r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !valid || i == 0 && (r == '.' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// emitAnonymousContainer lowers an anonymous `struct`/`union`/`enum`/`opaque`
// literal used in expression position (commonly `return struct { ... };`) to a
// module expression.
func (t *translator) emitAnonymousContainer() {
	if t.isKeyword("packed") {
		t.next()
	}
	kind := t.tok().Value
	t.next() // struct / union / opaque / enum

	if kind == "enum" {
		t.skipContainerTag()
		members := t.parseEnumMembers()
		t.write("(module {")
		for i, member := range members {
			if i > 0 {
				t.write(" ")
			}
			t.write("export const " + tsSafeMember(member) + " = " + quoteJS(member) + ";")
		}
		t.write("})")
		return
	}
	if kind == "opaque" {
		if t.isPunct("{") {
			t.skipBalanced()
		}
		t.write("(module {})")
		return
	}
	t.skipContainerTag()
	t.write("(module ")
	t.emitModuleBody()
	t.write(")")
}

// emitObjectLiteral lowers `.{ .a = 1 }` / `Type{ .a = 1 }` struct literals to
// object literals, and unnamed `.{ a, b }` / `Type{ a, b }` tuple literals to
// array literals.
func (t *translator) emitObjectLiteral() {
	if !t.expectPunct("{") {
		t.write("{}")
		return
	}
	// A named struct literal starts with `.field =`; anything else is a tuple.
	named := t.isPunct(".") && t.peek(1).Kind == zig_scanner.Identifier && t.peek(2).IsPunct("=")
	if named {
		var fields []string
		for !t.atEnd() && !t.isPunct("}") {
			if t.eatPunct(",") {
				continue
			}
			if t.isPunct("...") {
				t.next()
				fields = append(fields, "..."+t.capture(func() { t.emitExpr(0) }))
				continue
			}
			if t.eatPunct(".") {
				name := t.identifierText()
				if t.eatPunct("=") {
					value := t.capture(func() { t.emitExpr(0) })
					fields = append(fields, tsSafeMember(name)+": "+value)
				} else {
					fields = append(fields, tsSafeMember(name))
				}
				continue
			}
			// Unexpected token inside a named literal; skip to recover.
			before := t.i
			t.skipToCommaOrCloseBrace()
			if t.i == before {
				t.next()
			}
		}
		t.expectPunct("}")
		t.write("({ " + strings.Join(fields, ", ") + " })")
		return
	}
	// Tuple literal: lower to an array (Zig tuples are positional).
	var elements []string
	for !t.atEnd() && !t.isPunct("}") {
		if t.eatPunct(",") {
			continue
		}
		before := t.i
		elements = append(elements, t.capture(func() { t.emitExpr(0) }))
		if t.i == before {
			t.next()
		}
	}
	t.expectPunct("}")
	t.write("[" + strings.Join(elements, ", ") + "]")
}

func (t *translator) parseBuiltin() {
	tok := t.next() // builtin token
	name := tok.Value
	var args []string
	if t.eatPunct("(") {
		for !t.atEnd() && !t.isPunct(")") {
			if t.eatPunct(",") {
				continue
			}
			before := t.i
			args = append(args, t.capture(func() { t.emitExpr(0) }))
			if t.i == before {
				t.next()
			}
		}
		t.expectPunct(")")
	}

	switch name {
	case "import":
		// Inline `@import(...)` usage. Real module imports are emitted as import
		// declarations by emitVariable; anything left here (e.g. `builtin`,
		// `root`, or an inline use) has no module to resolve to.
		t.write("(globalThis as any)")
	case "hasDecl":
		if len(args) == 2 {
			t.write("(" + args[1] + " in (" + args[0] + " as any))")
			return
		}
		t.write("false")
	case "as":
		if len(args) == 2 {
			// A Zig `@as(T, v)` is a typed value. The first argument is a type
			// that may not have a valid TypeScript type expression equivalent
			// (e.g. `@Vector(3, T)`), so only the value is preserved.
			t.write("(" + args[1] + ")")
			return
		}
		t.write("undefined")
	case "intCast", "intFromBool", "intFromEnum", "enumFromInt", "floatCast", "floatFromInt",
		"intFromFloat", "ptrCast", "constCast", "bitCast", "truncate", "alignCast", "memcpy", "memset":
		if len(args) >= 1 {
			t.write(args[len(args)-1])
			return
		}
		t.write("undefined")
	case "sizeOf", "bitSizeOf", "alignOf", "offsetOf":
		t.write("0")
	case "typeName":
		t.write(`""`)
	case "This":
		// `@This()` is the enclosing type; there is no direct TypeScript
		// equivalent, so it degrades to an untyped value.
		t.write("({} as any)")
	case "field":
		if len(args) == 2 {
			t.write(args[0] + "[" + args[1] + "]")
			return
		}
		t.write("undefined")
	case "errorName":
		if len(args) == 1 {
			t.write(args[0] + ".message")
			return
		}
		t.write("undefined")
	case "TypeOf":
		if len(args) == 1 && isSimpleTypeName(args[0]) {
			t.write("typeof " + args[0])
			return
		}
		t.write("(globalThis as any)")
	case "compileError":
		t.write(`(() => { throw new Error("compile error"); })()`)
	default:
		// Unknown builtins (e.g. `@typeInfo`, `@tagName`) have no TypeScript
		// equivalent; call through a global so no unresolved helper name is
		// emitted.
		t.write("(globalThis as any).__zig" + name + "__(" + strings.Join(args, ", ") + ")")
	}
}

// parseImportSpecifier parses `@import("...")` and returns the module specifier.
func (t *translator) parseImportSpecifier() string {
	t.next() // @import
	spec := ""
	if t.eatPunct("(") {
		if t.tok().Kind == zig_scanner.String {
			spec = t.next().Value
		}
		for !t.atEnd() && !t.isPunct(")") {
			t.next()
		}
		t.eatPunct(")")
	}
	return spec
}

// skipBalancedOrExpr skips a balanced `(...)`/`{...}` group or a single token.
func (t *translator) skipBalancedOrExpr() {
	if t.isPunct("(") || t.isPunct("{") || t.isPunct("[") {
		t.skipBalanced()
		return
	}
	if !t.atEnd() {
		t.next()
	}
}

// --- Low level helpers ---

func (t *translator) identifierText() string {
	tok := t.tok()
	if tok.Kind == zig_scanner.Identifier || tok.Kind == zig_scanner.Keyword {
		t.next()
		return tsSafeIdent(tok.Value)
	}
	if !t.atEnd() {
		t.next()
	}
	return "_"
}

func (t *translator) skipBalanced() {
	var open, close string
	switch {
	case t.isPunct("("):
		open, close = "(", ")"
	case t.isPunct("{"):
		open, close = "{", "}"
	case t.isPunct("["):
		open, close = "[", "]"
	default:
		return
	}
	depth := 0
	for !t.atEnd() {
		switch {
		case t.isPunct(open):
			depth++
		case t.isPunct(close):
			depth--
			if depth == 0 {
				t.next()
				return
			}
		}
		t.next()
	}
}

func (t *translator) skipToSemicolon() {
	depth := 0
	for !t.atEnd() {
		tok := t.tok()
		if tok.Kind == zig_scanner.Punct {
			switch tok.Text {
			case "{", "(", "[":
				depth++
			case "}", ")", "]":
				if depth == 0 {
					return
				}
				depth--
			case ";":
				if depth == 0 {
					t.next()
					return
				}
			}
		}
		t.next()
	}
}

func (t *translator) skipEntry() {
	depth := 0
	for !t.atEnd() {
		tok := t.tok()
		if tok.Kind == zig_scanner.Punct {
			switch tok.Text {
			case "{", "(", "[":
				depth++
			case "}", ")", "]":
				if depth == 0 {
					return
				}
				depth--
			case ";":
				if depth == 0 {
					t.next()
					return
				}
			case ",":
				if depth == 0 {
					t.next()
					return
				}
			}
		}
		t.next()
	}
}

func (t *translator) skipToCommaOrCloseBrace() {
	depth := 0
	for !t.atEnd() {
		tok := t.tok()
		if tok.Kind == zig_scanner.Punct {
			switch tok.Text {
			case "{", "(", "[":
				depth++
			case "}", ")", "]":
				if depth == 0 {
					return
				}
				depth--
			case ",":
				if depth == 0 {
					return
				}
			}
		}
		t.next()
	}
}

// --- Naming / literal helpers ---

var numericTypes = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true, "i128": true, "isize": true,
	"u8": true, "u16": true, "u32": true, "u64": true, "u128": true, "usize": true,
	"f16": true, "f32": true, "f64": true, "f80": true, "f128": true,
	"comptime_int": true, "comptime_float": true,
	"c_char": true, "c_short": true, "c_ushort": true, "c_int": true, "c_uint": true,
	"c_long": true, "c_ulong": true, "c_longlong": true, "c_ulonglong": true,
	"c_longdouble": true,
}

// builtinTypeName maps a Zig builtin type name to its TypeScript equivalent.
func builtinTypeName(name string) (string, bool) {
	switch name {
	case "bool":
		return "boolean", true
	case "void":
		return "void", true
	case "noreturn":
		return "never", true
	case "anytype":
		return "any", true
	case "anyframe", "type", "anyopaque":
		return "unknown", true
	}
	if numericTypes[name] {
		return "number", true
	}
	return "", false
}

var reservedWords = map[string]bool{
	"break": true, "case": true, "catch": true, "class": true, "const": true,
	"continue": true, "debugger": true, "default": true, "delete": true, "do": true,
	"else": true, "enum": true, "export": true, "extends": true, "false": true,
	"finally": true, "for": true, "function": true, "if": true, "import": true,
	"in": true, "instanceof": true, "new": true, "null": true, "return": true,
	"super": true, "switch": true, "this": true, "throw": true, "true": true,
	"try": true, "typeof": true, "var": true, "void": true, "while": true,
	"with": true, "let": true, "static": true, "yield": true, "await": true,
}

func tsSafeIdent(name string) string {
	if name == "" {
		return "_"
	}
	if reservedWords[name] {
		return name + "_"
	}
	return sanitizeIdentifier(name)
}

func tsSafeMember(name string) string {
	if name == "" {
		return "_"
	}
	if reservedWords[name] {
		return name + "_"
	}
	return sanitizeIdentifier(name)
}

// sanitizeIdentifier turns an arbitrary Zig identifier (including quoted
// identifiers such as `@"PE32+"`) into a valid TypeScript identifier by
// replacing unsupported characters with `_` and prefixing leading digits.
func sanitizeIdentifier(name string) string {
	if name == "" {
		return "_"
	}
	var sb strings.Builder
	for _, r := range name {
		if r == '_' || r == '$' ||
			r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	result := sb.String()
	if result == "" {
		return "_"
	}
	if result[0] >= '0' && result[0] <= '9' {
		return "_" + result
	}
	return result
}

func sanitizeName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "test"
	}
	return sb.String()
}

// quoteJS wraps raw Zig string contents in a JavaScript double quoted string.
func quoteJS(value string) string {
	return `"` + escapeJSContent(value) + `"`
}

// quoteChar wraps the contents of a Zig character literal in a JavaScript
// double quoted string. Escape sequences already present in the literal are
// preserved, while unescaped quotes and control characters are escaped.
func quoteChar(value string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\\' && i+1 < len(value) {
			sb.WriteByte(c)
			i++
			sb.WriteByte(value[i])
			continue
		}
		switch c {
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// quoteMultilineJS joins Zig multiline string lines into a single escaped
// JavaScript string literal (which cannot contain literal line breaks).
func quoteMultilineJS(lines []string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for i, line := range lines {
		if i > 0 {
			sb.WriteString(`\n`)
		}
		for _, r := range line {
			switch r {
			case '"':
				sb.WriteString(`\"`)
			case '\\':
				sb.WriteString(`\\`)
			case '\n':
				sb.WriteString(`\n`)
			case '\r':
				sb.WriteString(`\r`)
			case '\t':
				sb.WriteString(`\t`)
			default:
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// escapeJSContent escapes a literal string so it can be placed inside a JS
// double quoted string. Escape sequences that are already present (from a Zig
// string literal) are intentionally left untouched unless they are Zig-specific.
func escapeJSContent(value string) string {
	value = strings.ReplaceAll(value, `\e`, `\x1b`)
	return value
}
