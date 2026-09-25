package zig_parser

import (
	"strconv"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// This file implements a permissive front-end for real Zig source. It does not attempt to type
// check Zig; instead it builds a well-formed TypeScript AST that captures the top-level shape of the
// file (declarations and function signatures) while lowering everything it does not model to
// `unknown`. The AST keeps the original source positions so hover/go-to-definition still work.
//
// The strict TypeScript dialect parser (parser.go + zig_struct.go) is tried first; this front-end is
// only used when that parser reports syntax errors, i.e. for genuine Zig. The approximation is
// intentionally silent: it is the supported path for arbitrary Zig and type-checks cleanly because
// unsupported declarations lower to `unknown`.

// parseZigSourceFile parses a real Zig source file into a permissive TypeScript AST.
func (p *Parser) parseZigSourceFile() *ast.SourceFile {
	pos := p.nodePos()
	var statements []*ast.Node
	if p.zigIsContainerBodyFile() {
		// A file whose top level is a bare container body (`field: Type = value,`) is itself a
		// container. Lower it to a `Self` class and default-export it so `@import` can resolve to
		// the file's type.
		statements = append(statements, p.zigParseSelfContainerFile()...)
	}
	for p.token != ast.KindEndOfFile {
		before := p.scanner.TokenFullStart()
		statements = append(statements, p.parseZigTopLevel()...)
		if p.scanner.TokenFullStart() == before {
			p.nextToken()
		}
	}
	if len(p.zigHoistedImports) != 0 {
		statements = append(p.zigHoistedImports, statements...)
		p.zigHoistedImports = nil
	}
	end := p.nodePos()
	endJSDoc := p.jsdocScannerInfo()
	eof := p.parseTokenNode()
	p.withJSDoc(eof, endJSDoc)
	if eof.Kind != ast.KindEndOfFile {
		panic("Expected end of file token from scanner.")
	}
	node := p.finishNode(p.factory.NewSourceFile(p.opts, p.sourceText, p.newNodeList(core.NewTextRange(pos, end), statements), eof), pos)
	result := node.AsSourceFile()
	p.finishSourceFile(result, false)
	// The permissive front-end approximates genuine Zig as best it can; its declarations are emitted
	// but not type-checked, so approximations do not surface as cascading errors. The strict dialect
	// parser (used for files it fully understands, such as the `src/**/*.zig` dialect) is still
	// checked normally.
	result.CheckJsDirective = &ast.CheckJsDirective{Enabled: false, Range: ast.CommentRange{TextRange: core.NewTextRange(pos, pos)}}
	collectExternalModuleReferences(result)
	return result
}

// zigIsIdent reports whether the current token is the contextual keyword `name`.
func (p *Parser) zigIsIdent(name string) bool {
	return p.token == ast.KindIdentifier && p.scanner.TokenValue() == name
}

// zigRecordTypeName records that `name` names a type or namespace in this file.
func (p *Parser) zigRecordTypeName(name string) {
	if p.zigTypeNames == nil {
		p.zigTypeNames = map[string]bool{}
	}
	p.zigTypeNames[name] = true
}

// zigRecordValueName records that `name` names a value (and not a type) in this file.
func (p *Parser) zigRecordValueName(name string) {
	if p.zigValueNames == nil {
		p.zigValueNames = map[string]bool{}
	}
	p.zigValueNames[name] = true
}

// zigIsKnownValue reports whether `name` is known to be a value and not a type/namespace.
func (p *Parser) zigIsKnownValue(name string) bool {
	return p.zigValueNames[name] && !p.zigTypeNames[name]
}

// zigBindingMayBeType reports whether an unannotated binding's initializer could be a type expression,
// in which case the binding is also exposed in the type namespace. Values with complex initializers
// (calls, container literals, literals, ...) are values only and must not get a bogus `unknown` alias.
func (p *Parser) zigBindingMayBeType(valueExpr *ast.Node) bool {
	if valueExpr == nil {
		return false
	}
	switch valueExpr.Kind {
	case ast.KindIdentifier:
		return !p.zigIsKnownValue(valueExpr.Text())
	case ast.KindPropertyAccessExpression:
		left := valueExpr
		for left.Kind == ast.KindPropertyAccessExpression {
			left = left.AsPropertyAccessExpression().Expression
		}
		return left.Kind == ast.KindIdentifier && p.zigTypeNames[left.Text()] && !p.zigIsKnownValue(left.Text())
	}
	return false
}

// parseZigTopLevel parses one top-level declaration, skipping anything it does not model.
func (p *Parser) parseZigTopLevel() []*ast.Node {
	pos := p.nodePos()
	exported := false
	for {
		switch p.token {
		case ast.KindExportKeyword: // `pub`
			exported = true
			p.nextToken()
			continue
		case ast.KindConstKeyword, ast.KindVarKeyword, ast.KindFunctionKeyword, ast.KindEnumKeyword, ast.KindModuleKeyword, ast.KindEndOfFile:
			// fall through to the switch below
		case ast.KindIdentifier:
			switch p.scanner.TokenValue() {
			case "export":
				exported = true
				p.nextToken()
				continue
			case "extern":
				p.nextToken()
				if p.token == ast.KindStringLiteral {
					p.nextToken()
				}
				continue
			case "inline", "noinline", "threadlocal":
				p.nextToken()
				continue
			case "linksection", "align", "callconv", "addrspace":
				p.nextToken()
				p.zigSkipBalanced(ast.KindOpenParenToken, ast.KindCloseParenToken)
				continue
			case "comptime":
				p.nextToken()
				if p.token == ast.KindOpenBraceToken {
					p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
					return nil
				}
				continue
			case "usingnamespace":
				p.zigSkipToSemicolon()
				return nil
			}
		}
		break
	}

	switch p.token {
	case ast.KindFunctionKeyword:
		return p.parseZigFunction(pos, exported)
	case ast.KindConstKeyword, ast.KindVarKeyword:
		return p.parseZigTopLevelBinding(pos, exported)
	case ast.KindEnumKeyword:
		// `const X = enum { ... }` is handled by the binding path; a bare `enum` is unusual.
		p.zigSkipToSemicolon()
		return nil
	}
	if p.zigIsIdent("test") {
		p.parseZigTest()
		return nil
	}
	p.zigSkipToSemicolonOrBlock()
	return nil
}

// parseZigFunction lowers a Zig function declaration. Parameters are captured by name; every type
// is lowered to `any` and the body is skipped.
func (p *Parser) parseZigFunction(pos int, exported bool) []*ast.Node {
	p.nextToken() // `fn`
	// Zig identifiers that tokenize as TypeScript keywords (e.g. `and`, `or`, `orelse` are scanned
	// as operators) still need a usable, unique function name. Synthesize one from the source text
	// and the position.
	name := p.factory.NewIdentifier("anonymous")
	if p.token == ast.KindIdentifier {
		name = p.parseIdentifier()
	} else if tokenIsIdentifierOrKeyword(p.token) || p.token == ast.KindStringLiteral {
		text := p.scanner.TokenValue()
		if text == "" {
			text = "fn"
		}
		p.nextToken()
		// Include the position so distinct keyword-named functions never collide.
		name = p.newIdentifierAt("_"+text+"_"+strconv.Itoa(pos), core.NewTextRange(pos, pos))
	}
	params := p.zigParseFunctionParameters()
	p.zigSkipPostParamModifiers()
	// A `fn Name(...) type` factory yields a Zig type; expose it in the type namespace too so uses
	// such as `Name(T)` in type position resolve.
	returnsType := p.token == ast.KindTypeKeyword
	returnType := p.zigParseReturnType(pos)
	body := p.zigSynthesizeFunctionBody(returnType, p.nodePos())

	result := p.finishNode(p.factory.NewFunctionDeclaration(
		p.zigExportModifiers(exported, pos),
		nil,
		name,
		nil,
		p.newNodeList(core.NewTextRange(pos, p.nodePos()), params),
		returnType,
		nil,
		body,
	), pos)
	p.checkJSSyntax(result)
	if !returnsType {
		p.zigRecordValueName(name.Text())
		return []*ast.Node{result}
	}
	p.zigRecordTypeName(name.Text())
	// Keep the synthesized alias bounded to its name: spanning the whole function would leave the
	// function's tokens in the alias node's trivia, which breaks language-service token navigation.
	typeAlias := p.finishNodeWithEnd(p.factory.NewTypeAliasDeclaration(
		p.zigExportModifiers(exported, pos),
		p.newIdentifierAt(zigTypeAliasName(name.Text()), name.Loc),
		nil,
		p.zigUnknownTypeAt(pos),
	), pos, name.End())
	return []*ast.Node{typeAlias, result}
}

// zigParseFunctionParameters parses a `( ... )` parameter list, returning the parameter nodes. It
// never spins: a parameter that consumes no tokens advances the scanner.
func (p *Parser) zigParseFunctionParameters() []*ast.Node {
	var params []*ast.Node
	usedParamNames := map[string]bool{}
	if p.token != ast.KindOpenParenToken {
		return params
	}
	p.nextToken()
	for p.token != ast.KindCloseParenToken && p.token != ast.KindEndOfFile {
		if p.token == ast.KindCommaToken {
			p.nextToken()
			continue
		}
		before := p.scanner.TokenFullStart()
		if param := p.parseZigParameter(usedParamNames); param != nil {
			params = append(params, param)
		}
		if p.token == ast.KindCommaToken {
			p.nextToken()
		}
		if p.scanner.TokenFullStart() == before {
			p.nextToken()
		}
	}
	if p.token == ast.KindCloseParenToken {
		p.nextToken()
	}
	return params
}

// zigSynthesizeFunctionBody consumes a function body (or a prototype `;`) and returns a block that
// satisfies the checker: a `throw` for `never`, otherwise a `return` of a value assignable to the
// declared return type.
func (p *Parser) zigSynthesizeFunctionBody(returnType *ast.Node, bodyStart int) *ast.Node {
	if p.token == ast.KindOpenBraceToken {
		p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
	} else if p.token == ast.KindSemicolonToken {
		p.nextToken()
	}
	bodyEnd := p.nodePos()
	var bodyStatement *ast.Node
	switch {
	case zigTypeIsKeywordNode(returnType, ast.KindNeverKeyword):
		bodyStatement = p.finishNodeWithEnd(p.factory.NewThrowStatement(p.zigUndefinedExpression(bodyStart)), bodyStart, bodyStart)
	case zigTypeIsKeywordNode(returnType, ast.KindVoidKeyword), zigTypeIsKeywordNode(returnType, ast.KindUnknownKeyword):
		bodyStatement = p.finishNodeWithEnd(p.factory.NewReturnStatement(p.newIdentifierAt("undefined", core.NewTextRange(bodyStart, bodyStart))), bodyStart, bodyStart)
	default:
		value := p.finishNode(p.factory.NewAsExpression(p.zigUndefinedExpression(bodyStart), returnType), bodyStart)
		bodyStatement = p.finishNodeWithEnd(p.factory.NewReturnStatement(value), bodyStart, bodyStart)
	}
	return p.finishNodeWithEnd(p.factory.NewBlock(
		p.newNodeList(core.NewTextRange(bodyStart, bodyEnd), []*ast.Node{bodyStatement}), true,
	), bodyStart, bodyEnd)
}

// parseZigParameter captures a single parameter. `comptime`/`noalias` modifiers are skipped.
// Parameter names are made safe for TypeScript: Zig identifiers that are TypeScript reserved words
// (`new`, `interface`, ...) are prefixed, and duplicate names (`_`, `_`) are disambiguated.
func (p *Parser) parseZigParameter(used map[string]bool) *ast.Node {
	pos := p.nodePos()
	for p.zigIsIdent("comptime") || p.zigIsIdent("noalias") {
		p.nextToken()
	}
	if p.token == ast.KindDotDotDotToken {
		p.nextToken()
		return p.finishNode(p.factory.NewParameterDeclaration(
			nil, nil, p.factory.NewIdentifier("args"), nil, p.zigUnknownTypeAt(pos), nil,
		), pos)
	}
	var name *ast.Node
	reserved := false
	// Zig identifiers are not TypeScript keywords, so `new`, `type`, `delete`, etc. are all legal
	// parameter names. Accept any identifier-or-keyword token here.
	if tokenIsIdentifierOrKeyword(p.token) {
		// Prefix every keyword token: some (`new`) are reserved words and some (`interface`) are
		// reserved in strict mode; both are illegal as TypeScript binding names.
		reserved = p.token != ast.KindIdentifier
		name = p.parseIdentifierName()
	}
	if p.zigIsIdent("anytype") || p.token == ast.KindTypeKeyword {
		p.nextToken()
	}
	paramType := p.zigUnknownTypeAt(pos)
	if p.token == ast.KindColonToken {
		p.nextToken()
		paramType = p.zigParseParameterType(pos)
	}

	base := "arg"
	if name != nil && name.Text() != "" {
		base = name.Text()
	}
	if reserved {
		base = "_" + base
	}
	final := base
	for used[final] {
		final += "_"
	}
	used[final] = true
	if name == nil || name.Text() != final {
		loc := core.NewTextRange(pos, pos)
		if name != nil {
			loc = name.Loc
		}
		name = p.newIdentifierAt(final, loc)
	}
	return p.finishNode(p.factory.NewParameterDeclaration(
		nil, nil, name, nil, paramType, nil,
	), pos)
}

// parseZigTopLevelBinding lowers a top-level `const`/`var`. Because Zig has first-class types, every
// binding is emitted as both a type alias and a value so it can be used on either side.
func (p *Parser) parseZigTopLevelBinding(pos int, exported bool) []*ast.Node {
	isConst := p.token == ast.KindConstKeyword
	p.nextToken()
	if !tokenIsIdentifierOrKeyword(p.token) {
		p.zigSkipToSemicolon()
		return nil
	}
	// Keywords are legal Zig binding names but not legal TypeScript binding names.
	nameIsKeyword := p.token != ast.KindIdentifier
	name := p.parseIdentifierName()
	if nameIsKeyword {
		name = p.newIdentifierAt("_"+name.Text()+"_"+strconv.Itoa(pos), name.Loc)
	}
	declaredType := (*ast.Node)(nil)
	if p.token == ast.KindColonToken {
		p.nextToken()
		declaredType = p.zigTryParseType(pos, func() bool {
			return p.token == ast.KindEqualsToken || p.token == ast.KindSemicolonToken
		})
	}
	var valueExpr *ast.Node
	if p.token == ast.KindEqualsToken {
		p.nextToken()
		if spec, ok := p.zigTryParseImportExpression(); ok {
			if p.token == ast.KindSemicolonToken {
				p.nextToken()
			}
			p.zigRecordTypeName(name.Text())
			return p.zigImportBindingDeclarations(name, spec, exported, pos)
		}
		if container := p.zigTryParseContainer(name, exported, pos); container != nil {
			p.zigRecordTypeName(name.Text())
			return container
		}
		if alias := p.zigTryParseQualifiedAlias(name, exported, pos, false); alias != nil {
			p.zigRecordTypeName(name.Text())
			return alias
		}
		// Preserve simple initializers (identifiers, property accesses, calls, literals) so the
		// language service can offer member completions and hovers inside them. Complex or
		// unsupported values are skipped as before.
		valueExpr = p.zigTryParseValueExpression()
		if valueExpr == nil {
			p.zigSkipValue()
		}
		// Remember bindings backed by `@typeInfo`, which are modeled as the tagged `Type` union
		// (`{ tag; data }`) so a `switch` over them can be lowered with tag/data narrowing.
		if valueExpr != nil && valueExpr.Kind == ast.KindCallExpression {
			if callee := valueExpr.AsCallExpression().Expression; callee != nil && callee.Kind == ast.KindIdentifier && callee.Text() == "typeInfo" {
				if p.zigTypeInfoNames == nil {
					p.zigTypeInfoNames = map[string]bool{}
				}
				p.zigTypeInfoNames[name.Text()] = true
			}
		}
	} else {
		p.zigSkipToSemicolon()
	}

	valueType := declaredType
	if valueType == nil && valueExpr == nil {
		// Leave the annotation off when a real initializer was preserved so its inferred type is
		// used (needed for member completions), otherwise fall back to `unknown`.
		valueType = p.zigUnknownTypeAt(pos)
	}
	initializer := valueExpr
	if initializer == nil {
		initializer = p.zigUnknownValueOfType(valueType, pos)
	}
	// End the declaration at the value (or the name when the value was not modelled) so the node
	// never spans source text that is not one of its children.
	end := name.End()
	if valueExpr != nil {
		end = valueExpr.End()
	}
	decl := p.finishNodeWithEnd(p.factory.NewVariableDeclaration(
		p.newIdentifierLike(name), nil, valueType, initializer,
	), pos, end)
	flags := ast.NodeFlagsLet
	if isConst {
		flags = ast.NodeFlagsConst
	}
	declList := p.finishNodeWithEnd(p.factory.NewVariableDeclarationList(
		p.newNodeList(core.NewTextRange(pos, end), []*ast.Node{decl}), flags,
	), pos, end)
	valueStatement := p.finishNodeWithEnd(p.factory.NewVariableStatement(
		p.zigExportModifiers(exported, pos), declList,
	), pos, end)

	// An unannotated binding may be a Zig type alias (`const X = SomeType;`), so also expose it in
	// the type namespace. Annotated bindings, and bindings whose initializer is clearly a value, are
	// values only and get no alias.
	if declaredType != nil || !p.zigBindingMayBeType(valueExpr) {
		p.zigRecordValueName(name.Text())
		return []*ast.Node{valueStatement}
	}
	// Bound the alias to its name; spanning the initializer would put the initializer's tokens in
	// the alias node's trivia, which breaks language-service token navigation.
	typeAlias := p.finishNodeWithEnd(p.factory.NewTypeAliasDeclaration(
		p.zigExportModifiers(exported, pos),
		p.newIdentifierAt(zigTypeAliasName(name.Text()), name.Loc),
		nil,
		p.zigUnknownTypeAt(pos),
	), pos, name.End())
	return []*ast.Node{typeAlias, valueStatement}
}

// zigTryParseImportExpression consumes `@import("spec")` and returns the specifier. On failure the
// parser is rewound.
func (p *Parser) zigTryParseImportExpression() (string, bool) {
	if p.token != ast.KindAtToken {
		return "", false
	}
	state := p.mark()
	p.nextToken()
	if !(tokenIsIdentifierOrKeyword(p.token) && p.scanner.TokenValue() == "import") {
		p.rewind(state)
		return "", false
	}
	p.nextToken()
	if p.token != ast.KindOpenParenToken {
		p.rewind(state)
		return "", false
	}
	p.nextToken()
	if p.token != ast.KindStringLiteral {
		p.rewind(state)
		return "", false
	}
	spec := p.scanner.TokenValue()
	p.nextToken()
	if p.token != ast.KindCloseParenToken {
		p.rewind(state)
		return "", false
	}
	p.nextToken()
	return spec, true
}

// zigTryParseQualifiedAlias recognizes `const X = A.B;` and lowers it to a TypeScript namespace
// alias (`import X = A.B;`), which preserves both namespace member access (`X.Y`) and type usage
// for the referenced entity. Only a bare qualified name terminated by the declaration separator is
// accepted; anything else is rewound.
func (p *Parser) zigTryParseQualifiedAlias(name *ast.Node, exported bool, pos int, container bool) []*ast.Node {
	if !tokenIsIdentifierOrKeyword(p.token) {
		return nil
	}
	state := p.mark()
	entity := p.newIdentifierLike(p.parseIdentifierName())
	for p.token == ast.KindDotToken {
		p.nextToken()
		if !tokenIsIdentifierOrKeyword(p.token) {
			p.rewind(state)
			return nil
		}
		right := p.parseIdentifierName()
		entity = p.finishNodeWithEnd(p.factory.NewQualifiedName(entity, p.newIdentifierLike(right)), entity.Pos(), right.End())
	}
	if entity.Kind != ast.KindQualifiedName {
		p.rewind(state)
		return nil
	}
	// `A.B` is only a namespace/type alias when `A` is a type or namespace. If `A` is a known value
	// (e.g. `const foo: Foo = ...; const x = foo.foo;`), this is a field access and must lower to a
	// value, not `import x = foo.foo`.
	if p.zigIsKnownValue(zigLeftmostName(entity)) {
		p.rewind(state)
		return nil
	}
	validEnd := p.token == ast.KindSemicolonToken
	if container {
		validEnd = validEnd || p.token == ast.KindCommaToken || p.token == ast.KindCloseBraceToken
	}
	if !validEnd {
		p.rewind(state)
		return nil
	}
	p.parseOptional(ast.KindSemicolonToken)
	importEquals := p.finishNodeWithEnd(p.factory.NewImportEqualsDeclaration(
		p.zigExportModifiers(exported, pos), false, p.newIdentifierLike(name), entity,
	), pos, p.nodePos())
	return []*ast.Node{importEquals}
}

// zigImportBindingDeclarations turns `const X = @import("spec")` into a real module import and, when
// the binding is exported, a matching re-export so `X` remains part of the public surface.
func (p *Parser) zigImportBindingDeclarations(name *ast.Node, spec string, exported bool, pos int) []*ast.Node {
	importDecl := p.zigImportDeclaration(name.Text(), spec, pos)
	if !exported {
		return []*ast.Node{importDecl}
	}
	exportName := p.newIdentifierAt(name.Text(), name.Loc)
	specifier := p.finishNode(p.factory.NewExportSpecifier(false, nil, exportName), pos)
	namedExports := p.finishNode(p.factory.NewNamedExports(
		p.newNodeList(core.NewTextRange(pos, pos), []*ast.Node{specifier}),
	), pos)
	exportDecl := p.finishNode(p.factory.NewExportDeclaration(nil, false, namedExports, nil, nil), pos)
	return []*ast.Node{importDecl, exportDecl}
}

// parseZigTest skips a `test "name" { ... }` declaration.
func (p *Parser) parseZigTest() {
	p.nextToken() // `test`
	if p.token == ast.KindStringLiteral || p.token == ast.KindIdentifier {
		p.nextToken()
	}
	if p.token == ast.KindOpenBraceToken {
		p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
	} else {
		p.zigSkipToSemicolon()
	}
}

// ---------------------------------------------------------------- type helpers

// zigReservedTypeNames are identifiers that TypeScript does not allow as type alias names
// (see TS2457). Zig code commonly uses these as constant names, so the synthesized alias is renamed
// while the value binding keeps the original name.
var zigReservedTypeNames = map[string]bool{
	"any": true, "unknown": true, "never": true, "void": true, "undefined": true, "null": true,
	"boolean": true, "number": true, "string": true, "symbol": true, "object": true,
	"bigint": true, "intrinsic": true,
}

// zigTypeAliasName returns a TypeScript-safe type alias name for a Zig binding name.
func zigTypeAliasName(name string) string {
	if zigReservedTypeNames[name] {
		return "_" + name
	}
	return name
}

// zigUnknownTypeAt creates an `unknown` type node anchored at pos. The permissive front-end never
// emits `any`, so unresolved declarations stay assignable but cannot be used unsafely.
func (p *Parser) zigUnknownTypeAt(pos int) *ast.Node {
	node := p.factory.NewKeywordTypeNode(ast.KindUnknownKeyword)
	node.Loc = core.NewTextRange(pos, pos)
	return node
}

// zigUndefinedExpression creates a permissive `null as unknown` value. It is zero-width at pos so it
// never spans source text it has no children for (which would break language-service navigation).
func (p *Parser) zigUndefinedExpression(pos int) *ast.Node {
	nullExpr := p.factory.NewToken(ast.KindNullKeyword)
	nullExpr.Loc = core.NewTextRange(pos, pos)
	return p.finishNodeWithEnd(p.factory.NewAsExpression(nullExpr, p.zigUnknownTypeAt(pos)), pos, pos)
}

// zigUnknownValueOfType creates a placeholder initializer assignable to the given type without
// exposing `any`: `null as unknown` when the type is itself permissive, otherwise cast to the type.
func (p *Parser) zigUnknownValueOfType(typ *ast.Node, pos int) *ast.Node {
	value := p.zigUndefinedExpression(pos)
	if typ == nil ||
		zigTypeIsKeywordNode(typ, ast.KindUnknownKeyword) ||
		zigTypeIsKeywordNode(typ, ast.KindAnyKeyword) ||
		zigTypeIsKeywordNode(typ, ast.KindVoidKeyword) {
		return value
	}
	return p.finishNodeWithEnd(p.factory.NewAsExpression(value, typ), pos, pos)
}

// zigTryParseValueExpression speculatively parses a binding initializer with the strict expression
// grammar and keeps it when it is a simple value form the language service can inspect (identifier,
// property/element access, call, literal, object/array literal). Recovered incomplete expressions
// (such as a trailing `.` while typing) are kept even though they produce a parse diagnostic, which
// is discarded. Returns nil (and rewinds) otherwise.
func (p *Parser) zigTryParseValueExpression() *ast.Node {
	if expression := p.zigTryParseSwitchExpression(); expression != nil {
		return expression
	}
	if expression := p.zigTryParseGenericValueExpression(); expression != nil {
		return expression
	}
	// The generic parse over-consumes a trailing `.` (it reads the next line's token as the property
	// name). Fall back to a line-aware member access so `foo.` while typing still yields a property
	// access node for the language service.
	return p.zigTryParseTrailingMemberAccess()
}

// zigNeverExpression creates an expression of type `never`, used to lower Zig's `unreachable`.
func (p *Parser) zigNeverExpression(pos int) *ast.Node {
	nullExpr := p.factory.NewToken(ast.KindNullKeyword)
	nullExpr.Loc = core.NewTextRange(pos, pos)
	neverType := p.factory.NewKeywordTypeNode(ast.KindNeverKeyword)
	neverType.Loc = core.NewTextRange(pos, pos)
	return p.finishNodeWithEnd(p.factory.NewAsExpression(nullExpr, neverType), pos, pos)
}

// zigSwitchPayload builds a fresh payload access for a switch arm. `@typeInfo` results are modeled as
// the tagged `Type` union (`{ tag; data }`), so their payload is `subject.data`.
func (p *Parser) zigSwitchPayload(subject *ast.Node, tag string, typeInfo bool, pos int) *ast.Node {
	subj := p.factory.DeepCloneReparse(subject)
	prop := tag
	if typeInfo {
		prop = "data"
	}
	return p.finishNode(p.factory.NewPropertyAccessExpression(subj, nil, p.newIdentifier(prop), ast.NodeFlagsNone), pos)
}

// zigSwitchCondition builds the condition that selects a switch arm. The tagged `Type` union is
// discriminated by `tag`; user unions expose each variant as a field of the same name.
func (p *Parser) zigSwitchCondition(subject *ast.Node, tag string, typeInfo bool, pos int) *ast.Node {
	subj := p.factory.DeepCloneReparse(subject)
	if typeInfo {
		tagAccess := p.finishNode(p.factory.NewPropertyAccessExpression(subj, nil, p.newIdentifier("tag"), ast.NodeFlagsNone), pos)
		literal := p.factory.NewStringLiteral(tag, ast.TokenFlagsNone)
		literal.Loc = core.NewTextRange(pos, pos)
		return p.finishNode(p.factory.NewBinaryExpression(nil, tagAccess, nil, p.factory.NewToken(ast.KindEqualsEqualsEqualsToken), literal), pos)
	}
	payload := p.finishNode(p.factory.NewPropertyAccessExpression(subj, nil, p.newIdentifier(tag), ast.NodeFlagsNone), pos)
	unknownPayload := p.finishNodeWithEnd(p.factory.NewAsExpression(payload, p.zigUnknownTypeAt(pos)), pos, pos)
	return p.finishNode(p.factory.NewBinaryExpression(
		nil,
		unknownPayload,
		nil,
		p.factory.NewToken(ast.KindExclamationEqualsEqualsToken),
		p.newIdentifier("undefined"),
	), pos)
}

// zigLowerSwitchArm lowers one switch arm body. `unreachable` becomes a `never` expression, and a
// `|capture|` payload is bound through an immediately-invoked arrow so the body keeps a real type.
func (p *Parser) zigLowerSwitchArm(body *ast.Node, subject *ast.Node, capture, tag string, typeInfo bool, pos int) *ast.Node {
	if body == nil {
		return nil
	}
	if body.Kind == ast.KindIdentifier && body.Text() == "unreachable" {
		return p.zigNeverExpression(pos)
	}
	if capture == "" || tag == "" {
		return body
	}
	if body.Kind == ast.KindIdentifier && body.Text() == capture {
		return p.zigSwitchPayload(subject, tag, typeInfo, pos)
	}
	payload := p.zigSwitchPayload(subject, tag, typeInfo, pos)
	param := p.finishNode(p.factory.NewParameterDeclaration(nil, nil, p.newIdentifier(capture), nil, nil, nil), pos)
	params := p.newNodeList(core.NewTextRange(pos, pos), []*ast.Node{param})
	arrowToken := p.factory.NewToken(ast.KindEqualsGreaterThanToken)
	arrowToken.Loc = core.NewTextRange(pos, pos)
	arrow := p.finishNode(p.factory.NewArrowFunction(nil, nil, params, nil, nil, arrowToken, body), pos)
	paren := p.finishNode(p.factory.NewParenthesizedExpression(arrow), pos)
	return p.finishNode(p.factory.NewCallExpression(paren, nil, nil, p.newNodeList(core.NewTextRange(pos, pos), []*ast.Node{payload}), ast.NodeFlagsNone), pos)
}

// zigTryParseSwitchExpression parses `switch (subject) { .tag => |cap| body, ..., else => body }` and
// lowers it to a conditional expression so a binding keeps a real inferred type instead of `unknown`.
func (p *Parser) zigTryParseSwitchExpression() *ast.Node {
	if p.token != ast.KindSwitchKeyword {
		return nil
	}
	state := p.mark()
	pos := p.nodePos()
	p.nextToken() // switch
	if p.token != ast.KindOpenParenToken {
		p.rewind(state)
		return nil
	}
	p.nextToken()
	subject := p.parseAssignmentExpressionOrHigher()
	if subject == nil || p.token != ast.KindCloseParenToken {
		p.rewind(state)
		return nil
	}
	subjectIsTypeInfo := subject.Kind == ast.KindIdentifier && p.zigTypeInfoNames[subject.Text()]
	p.nextToken()
	if p.token != ast.KindOpenBraceToken {
		p.rewind(state)
		return nil
	}
	p.nextToken()

	type switchArm struct {
		tag    string
		body   *ast.Node
		isElse bool
	}
	var arms []switchArm
	for p.token != ast.KindCloseBraceToken && p.token != ast.KindEndOfFile {
		if p.parseOptional(ast.KindCommaToken) {
			continue
		}
		var arm switchArm
		switch {
		case p.token == ast.KindElseKeyword:
			arm.isElse = true
			p.nextToken()
		case p.token == ast.KindDotToken:
			p.nextToken()
			if p.token != ast.KindIdentifier && p.token != ast.KindAtToken {
				p.rewind(state)
				return nil
			}
			arm.tag = p.parseZigIdentifierName().Text()
		default:
			p.rewind(state)
			return nil
		}
		if p.token != ast.KindEqualsGreaterThanToken {
			p.rewind(state)
			return nil
		}
		p.nextToken()
		capture := ""
		if p.token == ast.KindBarToken {
			p.nextToken()
			if !tokenIsIdentifierOrKeyword(p.token) {
				p.rewind(state)
				return nil
			}
			capture = p.parseIdentifierName().Text()
			if p.token != ast.KindBarToken {
				p.rewind(state)
				return nil
			}
			p.nextToken()
		}
		body := p.parseAssignmentExpressionOrHigher()
		arm.body = p.zigLowerSwitchArm(body, subject, capture, arm.tag, subjectIsTypeInfo, pos)
		arms = append(arms, arm)
		p.parseOptional(ast.KindCommaToken)
	}
	if p.token != ast.KindCloseBraceToken {
		p.rewind(state)
		return nil
	}
	p.nextToken()

	var result *ast.Node
	for i := len(arms) - 1; i >= 0; i-- {
		arm := arms[i]
		if result == nil || arm.isElse {
			result = arm.body
			continue
		}
		cond := p.zigSwitchCondition(subject, arm.tag, subjectIsTypeInfo, pos)
		result = p.finishNode(p.factory.NewConditionalExpression(cond, nil, arm.body, nil, result), pos)
	}
	if result == nil {
		p.rewind(state)
		return nil
	}
	return p.finishNodeWithEnd(result, pos, p.nodePos())
}

// zigTryParseGenericValueExpression is the strict-parser-based value parse.
func (p *Parser) zigTryParseGenericValueExpression() *ast.Node {
	state := p.mark()
	before := p.scanner.TokenFullStart()
	expression := p.parseAssignmentExpressionOrHigher()
	progressed := p.scanner.TokenFullStart() != before
	atBoundary := p.token == ast.KindSemicolonToken || p.token == ast.KindEndOfFile ||
		p.token == ast.KindCommaToken || p.token == ast.KindCloseBraceToken
	if expression != nil && progressed && atBoundary && isSimpleZigValueExpression(expression) {
		// Keep the recovered expression but drop the speculative diagnostics it produced.
		p.diagnostics = p.diagnostics[:state.diagnosticsLen]
		p.jsDiagnostics = p.jsDiagnostics[:state.jsDiagnosticsLen]
		p.jsdocInfos = p.jsdocInfos[:state.jsdocInfosLen]
		p.reparsedClones = p.reparsedClones[:state.reparsedClonesLen]
		p.hasParseError = state.hasParseError
		return expression
	}
	p.rewind(state)
	return nil
}

// zigTryParseTrailingMemberAccess parses `a.b.` (a member access whose final `.` is followed by a
// line break or boundary, as while typing) without consuming the next line's tokens. Returns nil
// when the input does not start with such an access.
func (p *Parser) zigTryParseTrailingMemberAccess() *ast.Node {
	if !tokenIsIdentifierOrKeyword(p.token) {
		return nil
	}
	state := p.mark()
	expression := p.newIdentifierLike(p.parseIdentifierName())
	for p.token == ast.KindDotToken {
		hasName := p.lookAhead(func(pp *Parser) bool {
			return pp.nextToken() != ast.KindEndOfFile && tokenIsIdentifierOrKeyword(pp.token) && !pp.hasPrecedingLineBreak()
		})
		dotPos := p.nodePos()
		p.nextToken() // consume `.`
		if hasName {
			name := p.parseIdentifierName()
			expression = p.finishNodeWithEnd(p.factory.NewPropertyAccessExpression(expression, nil, name, ast.NodeFlagsNone), expression.Pos(), p.nodePos())
			continue
		}
		missing := p.newIdentifierAt("", core.NewTextRange(dotPos, dotPos))
		expression = p.finishNodeWithEnd(p.factory.NewPropertyAccessExpression(expression, nil, missing, ast.NodeFlagsNone), expression.Pos(), p.nodePos())
		return expression
	}
	p.rewind(state)
	return nil
}

// isSimpleZigValueExpression reports whether an expression is simple enough to preserve in the
// permissive AST.
func isSimpleZigValueExpression(node *ast.Node) bool {
	switch node.Kind {
	case ast.KindIdentifier, ast.KindPropertyAccessExpression, ast.KindElementAccessExpression,
		ast.KindCallExpression, ast.KindParenthesizedExpression,
		ast.KindStringLiteral, ast.KindNumericLiteral, ast.KindBigIntLiteral,
		ast.KindTrueKeyword, ast.KindFalseKeyword, ast.KindNullKeyword,
		ast.KindObjectLiteralExpression, ast.KindArrayLiteralExpression,
		ast.KindAsExpression, ast.KindNonNullExpression:
		return true
	}
	return false
}

// zigVoidTypeAt creates a `void` keyword type anchored at pos.
func (p *Parser) zigVoidTypeAt(pos int) *ast.Node {
	node := p.factory.NewKeywordTypeNode(ast.KindVoidKeyword)
	node.Loc = core.NewTextRange(pos, pos)
	return node
}

// zigTypeIsKeywordNode reports whether the type node is the given keyword type (e.g. `never`).
func zigTypeIsKeywordNode(node *ast.Node, kind ast.Kind) bool {
	return node != nil && node.Kind == kind
}

// zigNormalizeType maps Zig type syntax with no TypeScript equivalent onto `unknown`.
func (p *Parser) zigNormalizeType(typ *ast.Node, pos int) *ast.Node {
	if typ != nil && typ.Kind == ast.KindTypeReference {
		if name := typ.AsTypeReferenceNode().TypeName; name != nil {
			if name.Kind == ast.KindIdentifier {
				switch name.Text() {
				case "error", "anyerror", "anyopaque", "anyframe", "anytype", "type":
					return p.zigUnknownTypeAt(pos)
				}
			} else if name.Kind == ast.KindQualifiedName {
				switch zigLeftmostName(name) {
				case "builtin", "root":
					// Compile-time-only pseudo modules have no declarations to resolve against.
					return p.zigUnknownTypeAt(pos)
				}
			}
		}
	}
	return typ
}

// zigLeftmostName returns the leftmost identifier of a (possibly qualified) entity name.
func zigLeftmostName(name *ast.Node) string {
	for name != nil && name.Kind == ast.KindQualifiedName {
		name = name.AsQualifiedName().Left
	}
	if name != nil && name.Kind == ast.KindIdentifier {
		return name.Text()
	}
	return ""
}

// zigTryParseType speculatively parses a Zig type using the strict parser's type grammar. It is only
// accepted when the type is consumed cleanly (no diagnostics) and the parser lands on a token for
// which isBoundary reports true. Otherwise the parser is rewound and nil is returned. Speculative
// diagnostics are always discarded so the permissive front-end stays silent.
func (p *Parser) zigTryParseType(pos int, isBoundary func() bool) *ast.Node {
	state := p.mark()
	before := p.scanner.TokenFullStart()
	candidate := p.parseType()
	// Named error-union types (`E!T`, `anyerror!T`) leave the `!` behind because `parseType` only
	// handles the leading-`!` form; keep only the payload type.
	if p.token == ast.KindExclamationToken {
		p.nextToken()
		if payload := p.parseType(); payload != nil {
			candidate = payload
		}
	}
	if candidate != nil && p.scanner.TokenFullStart() != before && len(p.diagnostics) == state.diagnosticsLen && isBoundary() {
		return p.zigNormalizeType(candidate, pos)
	}
	p.rewind(state)
	return nil
}

// zigParseParameterType parses a parameter's type annotation, falling back to `unknown` and skipping
// the type when it is not modelled.
func (p *Parser) zigParseParameterType(pos int) *ast.Node {
	if typ := p.zigTryParseType(pos, func() bool {
		return p.token == ast.KindCommaToken || p.token == ast.KindCloseParenToken
	}); typ != nil {
		return typ
	}
	p.zigSkipTypeUntil(ast.KindCommaToken, ast.KindCloseParenToken)
	return p.zigUnknownTypeAt(pos)
}

// zigParseReturnType parses a function's return type, defaulting to `void` when omitted and falling
// back to skipping the type (as `unknown`) when it is not modelled.
func (p *Parser) zigParseReturnType(pos int) *ast.Node {
	if p.token == ast.KindOpenBraceToken || p.token == ast.KindSemicolonToken || p.token == ast.KindEndOfFile {
		return p.zigVoidTypeAt(pos)
	}
	if typ := p.zigTryParseType(pos, func() bool {
		return p.token == ast.KindOpenBraceToken || p.token == ast.KindSemicolonToken || p.token == ast.KindEndOfFile
	}); typ != nil {
		return typ
	}
	p.zigSkipReturnType()
	return p.zigUnknownTypeAt(pos)
}

func (p *Parser) zigEmptyBlockAt(pos int) *ast.Node {
	return p.finishNode(p.factory.NewBlock(p.newNodeList(core.NewTextRange(pos, pos), nil), true), pos)
}

// --------------------------------------------------------------- skip helpers

// zigSkipBalanced consumes a balanced open/close delimited region.
func (p *Parser) zigSkipBalanced(open ast.Kind, close ast.Kind) {
	if p.token != open {
		return
	}
	depth := 0
	for p.token != ast.KindEndOfFile {
		switch p.token {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				p.nextToken()
				return
			}
		}
		p.nextToken()
	}
}

// zigSkipToSemicolon skips to and consumes the next top-level semicolon.
func (p *Parser) zigSkipToSemicolon() {
	depth := 0
	for p.token != ast.KindEndOfFile {
		switch p.token {
		case ast.KindOpenParenToken, ast.KindOpenBracketToken, ast.KindOpenBraceToken:
			depth++
		case ast.KindCloseParenToken, ast.KindCloseBracketToken, ast.KindCloseBraceToken:
			if depth == 0 {
				return
			}
			depth--
		case ast.KindSemicolonToken:
			if depth == 0 {
				p.nextToken()
				return
			}
		}
		p.nextToken()
	}
}

// zigSkipToSemicolonOrBlock skips an unrecognized declaration.
func (p *Parser) zigSkipToSemicolonOrBlock() {
	depth := 0
	for p.token != ast.KindEndOfFile {
		switch p.token {
		case ast.KindOpenParenToken, ast.KindOpenBracketToken:
			depth++
		case ast.KindCloseParenToken, ast.KindCloseBracketToken:
			if depth > 0 {
				depth--
			}
		case ast.KindOpenBraceToken:
			if depth == 0 {
				p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
				return
			}
			depth++
		case ast.KindCloseBraceToken:
			if depth > 0 {
				depth--
			} else {
				return
			}
		case ast.KindSemicolonToken:
			if depth == 0 {
				p.nextToken()
				return
			}
		}
		p.nextToken()
	}
}

// zigSkipPostParamModifiers skips `align(...)`, `callconv(...)`, etc. after a parameter list.
func (p *Parser) zigSkipPostParamModifiers() {
	for p.zigIsIdent("align") || p.zigIsIdent("callconv") || p.zigIsIdent("linksection") || p.zigIsIdent("addrspace") {
		p.nextToken()
		p.zigSkipBalanced(ast.KindOpenParenToken, ast.KindCloseParenToken)
	}
}

// zigSkipReturnType skips a return type up to the function body `{` or `;`.
func (p *Parser) zigSkipReturnType() {
	depth := 0
	prevWasError := false
	for p.token != ast.KindEndOfFile {
		switch p.token {
		case ast.KindOpenParenToken, ast.KindOpenBracketToken:
			depth++
		case ast.KindCloseParenToken, ast.KindCloseBracketToken:
			if depth > 0 {
				depth--
			} else {
				return
			}
		case ast.KindOpenBraceToken:
			if depth == 0 && !prevWasError {
				return
			}
			depth++
		case ast.KindCloseBraceToken:
			if depth > 0 {
				depth--
			} else {
				return
			}
		case ast.KindSemicolonToken:
			if depth == 0 {
				return
			}
		}
		prevWasError = p.token == ast.KindIdentifier && p.scanner.TokenValue() == "error"
		p.nextToken()
	}
}

// zigSkipFunctionBody skips a function body (or consumes a prototype semicolon).
func (p *Parser) zigSkipFunctionBody() {
	if p.token == ast.KindOpenBraceToken {
		p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
		return
	}
	if p.token == ast.KindSemicolonToken {
		p.nextToken()
	}
}

// zigSkipValue skips a value initializer up to and including the next top-level semicolon.
func (p *Parser) zigSkipValue() {
	depth := 0
	prevWasError := false
	for p.token != ast.KindEndOfFile {
		switch p.token {
		case ast.KindOpenParenToken, ast.KindOpenBracketToken:
			depth++
		case ast.KindCloseParenToken, ast.KindCloseBracketToken:
			if depth > 0 {
				depth--
			} else {
				return
			}
		case ast.KindOpenBraceToken:
			if depth == 0 && prevWasError {
				// `error{ ... }` type braces, not a block
				p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
				continue
			}
			depth++
		case ast.KindCloseBraceToken:
			if depth > 0 {
				depth--
			} else {
				return
			}
		case ast.KindSemicolonToken:
			if depth == 0 {
				p.nextToken()
				return
			}
		}
		prevWasError = p.token == ast.KindIdentifier && p.scanner.TokenValue() == "error"
		p.nextToken()
	}
}

// zigSkipTypeUntil skips a type up to (but not including) one of the given top-level stop tokens.
func (p *Parser) zigSkipTypeUntil(stops ...ast.Kind) {
	depth := 0
	for p.token != ast.KindEndOfFile {
		if depth == 0 {
			for _, stop := range stops {
				if p.token == stop {
					return
				}
			}
		}
		switch p.token {
		case ast.KindOpenParenToken, ast.KindOpenBracketToken:
			depth++
		case ast.KindCloseParenToken, ast.KindCloseBracketToken:
			if depth > 0 {
				depth--
			} else {
				return
			}
		case ast.KindOpenBraceToken:
			// A brace in a type position is part of the type, e.g. `switch (...) { ... }`,
			// `struct { ... }` or `error{ ... }`. Skip the balanced block rather than mistaking it
			// for the start of a function body.
			p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
			continue
		case ast.KindCloseBraceToken:
			if depth > 0 {
				depth--
			} else {
				return
			}
		}
		p.nextToken()
	}
}
