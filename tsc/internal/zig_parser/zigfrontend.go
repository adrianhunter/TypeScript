package zig_parser

import (
	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/diagnostics"
)

// This file implements a permissive front-end for real Zig source. It does not attempt to type
// check Zig; instead it builds a well-formed TypeScript AST that captures the top-level shape of the
// file (declarations and function signatures) while lowering everything it does not model to
// `unknown`. The AST keeps the original source positions so hover/go-to-definition still work.
//
// The strict TypeScript dialect parser (parser.go + zig_struct.go) is tried first; this front-end is
// only used when that parser reports syntax errors, i.e. for genuine Zig. Falling back is reported as
// a compiler error so unsupported constructs are never silently accepted.

// parseZigSourceFile parses a real Zig source file into a permissive TypeScript AST.
func (p *Parser) parseZigSourceFile() *ast.SourceFile {
	pos := p.nodePos()
	var statements []*ast.Node
	for p.token != ast.KindEndOfFile {
		before := p.scanner.TokenFullStart()
		statements = append(statements, p.parseZigTopLevel()...)
		if p.scanner.TokenFullStart() == before {
			p.nextToken()
		}
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
	// The strict parser could not model this file, so the declarations below are only an
	// approximation. Surface an error instead of silently emitting `any`.
	p.diagnostics = append(p.diagnostics, ast.NewDiagnosticFromText(
		nil,
		core.NewTextRange(pos, pos),
		diagnostics.CodeZigFileNotFullySupported,
		diagnostics.CategoryError,
		"Zig file contains constructs that are not fully supported; declarations are approximated as 'unknown'.",
		nil, nil, false, false,
	))
	p.finishSourceFile(result, false)
	collectExternalModuleReferences(result)
	return result
}

// zigIsIdent reports whether the current token is the contextual keyword `name`.
func (p *Parser) zigIsIdent(name string) bool {
	return p.token == ast.KindIdentifier && p.scanner.TokenValue() == name
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
		return []*ast.Node{p.parseZigFunction(pos, exported)}
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
func (p *Parser) parseZigFunction(pos int, exported bool) *ast.Node {
	p.nextToken() // `fn`
	name := p.factory.NewIdentifier("anonymous")
	if p.token == ast.KindIdentifier {
		name = p.parseIdentifier()
	}
	var params []*ast.Node
	if p.token == ast.KindOpenParenToken {
		p.nextToken()
		for p.token != ast.KindCloseParenToken && p.token != ast.KindEndOfFile {
			if p.token == ast.KindCommaToken {
				p.nextToken()
				continue
			}
			if param := p.parseZigParameter(); param != nil {
				params = append(params, param)
			}
			if p.token == ast.KindCommaToken {
				p.nextToken()
			}
		}
		if p.token == ast.KindCloseParenToken {
			p.nextToken()
		}
	}
	p.zigSkipPostParamModifiers()
	p.zigSkipReturnType()
	var body *ast.Node
	if p.token == ast.KindOpenBraceToken {
		bodyStart := p.nodePos()
		p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
		bodyEnd := p.nodePos()
		body = p.finishNodeWithEnd(p.factory.NewBlock(p.newNodeList(core.NewTextRange(bodyStart, bodyEnd), nil), true), bodyStart, bodyEnd)
	} else if p.token == ast.KindSemicolonToken {
		p.nextToken()
	}

	result := p.finishNode(p.factory.NewFunctionDeclaration(
		p.zigExportModifiers(exported, pos),
		nil,
		name,
		nil,
		p.newNodeList(core.NewTextRange(pos, p.nodePos()), params),
		p.zigUnknownTypeAt(p.nodePos()),
		nil,
		body,
	), pos)
	p.checkJSSyntax(result)
	return result
}

// parseZigParameter captures a single parameter. `comptime`/`noalias` modifiers are skipped.
func (p *Parser) parseZigParameter() *ast.Node {
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
	if p.token == ast.KindIdentifier {
		name = p.parseIdentifier()
	}
	if p.zigIsIdent("anytype") || p.token == ast.KindTypeKeyword {
		p.nextToken()
	}
	if p.token == ast.KindColonToken {
		p.nextToken()
		p.zigSkipTypeUntil(ast.KindCommaToken, ast.KindCloseParenToken)
	}
	if name == nil {
		name = p.factory.NewIdentifier("arg")
	}
	return p.finishNode(p.factory.NewParameterDeclaration(
		nil, nil, name, nil, p.zigUnknownTypeAt(pos), nil,
	), pos)
}

// parseZigTopLevelBinding lowers a top-level `const`/`var`. Because Zig has first-class types, every
// binding is emitted as both a type alias and a value so it can be used on either side.
func (p *Parser) parseZigTopLevelBinding(pos int, exported bool) []*ast.Node {
	isConst := p.token == ast.KindConstKeyword
	p.nextToken()
	if p.token != ast.KindIdentifier {
		p.zigSkipToSemicolon()
		return nil
	}
	name := p.parseIdentifier()
	if p.token == ast.KindColonToken {
		p.nextToken()
		p.zigSkipTypeUntil(ast.KindEqualsToken, ast.KindSemicolonToken)
	}
	if p.token == ast.KindEqualsToken {
		p.nextToken()
		p.zigSkipValue()
	} else {
		p.zigSkipToSemicolon()
	}

	typeAlias := p.finishNode(p.factory.NewTypeAliasDeclaration(
		p.zigExportModifiers(exported, pos),
		p.newIdentifierLike(name),
		nil,
		p.zigUnknownTypeAt(pos),
	), pos)

	decl := p.finishNode(p.factory.NewVariableDeclaration(
		p.newIdentifierLike(name), nil, p.zigUnknownTypeAt(pos), p.zigUndefinedExpression(pos),
	), pos)
	flags := ast.NodeFlagsLet
	if isConst {
		flags = ast.NodeFlagsConst
	}
	declList := p.finishNode(p.factory.NewVariableDeclarationList(
		p.newNodeList(core.NewTextRange(pos, p.nodePos()), []*ast.Node{decl}), flags,
	), pos)
	valueStatement := p.finishNode(p.factory.NewVariableStatement(
		p.zigExportModifiers(exported, pos), declList,
	), pos)

	return []*ast.Node{typeAlias, valueStatement}
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

// zigUnknownTypeAt creates an `unknown` type node anchored at pos. The permissive front-end never
// emits `any`, so unresolved declarations stay assignable but cannot be used unsafely.
func (p *Parser) zigUnknownTypeAt(pos int) *ast.Node {
	node := p.factory.NewKeywordTypeNode(ast.KindUnknownKeyword)
	node.Loc = core.NewTextRange(pos, pos)
	return node
}

// zigUndefinedExpression creates a permissive `null as unknown` value.
func (p *Parser) zigUndefinedExpression(pos int) *ast.Node {
	nullExpr := p.factory.NewToken(ast.KindNullKeyword)
	nullExpr.Loc = core.NewTextRange(pos, pos)
	return p.finishNode(p.factory.NewAsExpression(nullExpr, p.zigUnknownTypeAt(pos)), pos)
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
	prevWasError := false
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
			if depth == 0 && prevWasError {
				p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
				continue
			}
			// A brace here starts the declaration's implementation; stop.
			if depth == 0 {
				return
			}
			depth++
		case ast.KindCloseBraceToken:
			if depth > 0 {
				depth--
			} else {
				return
			}
		}
		prevWasError = p.token == ast.KindIdentifier && p.scanner.TokenValue() == "error"
		p.nextToken()
	}
}
