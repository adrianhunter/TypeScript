package zig_parser

import (
	"strconv"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// This file lowers Zig containers (`struct`, `enum`, `union`, `opaque`) into the same
// interface/namespace/type shapes the strict dialect parser produces, so that declarations such as
// `Container.Member` and field/method types are preserved in the emitted `.d.ts`.

// zigIsContainerBodyFile reports whether a file's top level is a bare container body, i.e. it starts
// with `field: Type`. Such a file is itself a container (named `Self`).
func (p *Parser) zigIsContainerBodyFile() bool {
	if !tokenIsIdentifierOrKeyword(p.token) {
		return false
	}
	return p.lookAhead(func(pp *Parser) bool {
		return pp.nextToken() == ast.KindColonToken
	})
}

// zigParseSelfContainerFile lowers a bare container-body file to a `Self` class (plus a `Self`
// namespace for non-field members) and default-exports it, so importers can bind the file to its
// type via a default import.
func (p *Parser) zigParseSelfContainerFile() []*ast.Node {
	members := p.zigParseContainerMembers()
	var classMembers, namespaceMembers []*ast.Node
	for _, member := range members {
		if member.Kind == ast.KindPropertyDeclaration {
			classMembers = append(classMembers, member)
			continue
		}
		namespaceMembers = append(namespaceMembers, member)
	}
	end := p.nodePos()
	loc := core.NewTextRange(0, len(p.sourceText))
	synth := core.NewTextRange(-1, -1)
	out := []*ast.Node{p.finishNode(p.factory.NewClassDeclaration(
		nil, p.newIdentifierAt("Self", synth), nil, nil, p.newNodeList(loc, classMembers),
	), 0)}
	if len(namespaceMembers) > 0 {
		moduleBlock := p.finishNodeWithEnd(p.factory.NewModuleBlock(p.newNodeList(loc, namespaceMembers)), 0, end)
		namespace := p.finishNodeWithEnd(p.factory.NewModuleDeclaration(
			nil, ast.KindModuleKeyword, p.newIdentifierAt("Self", synth), nil, moduleBlock,
		), 0, end)
		namespace.Flags |= ast.NodeFlagsModuleFragment
		out = append(out, namespace)
	}
	// An export assignment is already an export; it must not also carry an export modifier.
	out = append(out, p.finishNodeWithEnd(p.factory.NewExportAssignment(
		nil, false, nil, p.newIdentifierAt("Self", synth),
	), 0, end))
	return out
}

// zigTryParseContainer parses a container literal used as a binding initializer and returns its
// lowered declarations. It returns nil (and rewinds) when the current token does not start one.
func (p *Parser) zigTryParseContainer(name *ast.Node, exported bool, pos int) []*ast.Node {
	state := p.mark()
	for p.zigIsIdent("packed") || p.zigIsIdent("extern") {
		p.nextToken()
	}
	kind := ast.KindUnknown
	switch {
	case p.token == ast.KindModuleKeyword: // the scanner maps `struct` to `module`
		kind = ast.KindModuleKeyword
		p.nextToken()
	case p.token == ast.KindEnumKeyword:
		kind = ast.KindEnumKeyword
		p.nextToken()
	case p.zigIsIdent("opaque"):
		p.nextToken()
		if p.token == ast.KindOpenBraceToken {
			p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
		}
		return p.zigOpaqueContainer(name, exported, pos)
	case p.zigIsIdent("union"):
		kind = ast.KindModuleKeyword
		p.nextToken()
	default:
		p.rewind(state)
		return nil
	}
	// Optional backing type / enum tag: `enum(u8)`, `union(enum)`.
	if p.token == ast.KindOpenParenToken {
		p.zigSkipBalanced(ast.KindOpenParenToken, ast.KindCloseParenToken)
	}
	if p.token != ast.KindOpenBraceToken {
		p.rewind(state)
		return nil
	}
	p.nextToken()

	if kind == ast.KindEnumKeyword {
		members := p.zigParseEnumMembers()
		end := p.nodePos()
		if p.token == ast.KindCloseBraceToken {
			p.nextToken()
			end = p.nodePos()
		}
		return p.zigEnumDeclarations(name, members, exported, pos, end)
	}

	members := p.zigParseContainerMembers()
	end := p.nodePos()
	if p.token == ast.KindCloseBraceToken {
		p.nextToken()
		end = p.nodePos()
	}
	moduleBlock := p.finishNodeWithEnd(p.factory.NewModuleBlock(
		p.newNodeList(core.NewTextRange(pos, end), members),
	), pos, end)
	return p.zigContainerDeclarations(name, moduleBlock, exported, pos, end)
}

// zigOpaqueContainer lowers `opaque {}` to an empty interface plus namespace.
func (p *Parser) zigOpaqueContainer(name *ast.Node, exported bool, pos int) []*ast.Node {
	end := p.nodePos()
	iface := p.finishNodeWithEnd(p.factory.NewInterfaceDeclaration(
		p.zigExportModifiers(exported, pos), p.newIdentifierLike(name), nil, nil,
		p.newNodeList(core.NewTextRange(pos, end), nil),
	), pos, end)
	moduleBlock := p.finishNodeWithEnd(p.factory.NewModuleBlock(
		p.newNodeList(core.NewTextRange(pos, end), nil),
	), pos, end)
	moduleDecl := p.finishNodeWithEnd(p.factory.NewModuleDeclaration(
		p.zigExportModifiers(exported, pos), ast.KindModuleKeyword, p.newIdentifierLike(name), nil, moduleBlock,
	), pos, end)
	moduleDecl.Flags |= ast.NodeFlagsModuleFragment
	return []*ast.Node{iface, moduleDecl}
}

// zigParseEnumMembers collects the member names of an `enum { ... }` container.
func (p *Parser) zigParseEnumMembers() []string {
	var members []string
	for p.token != ast.KindCloseBraceToken && p.token != ast.KindEndOfFile {
		if p.parseOptional(ast.KindCommaToken) || p.parseOptional(ast.KindSemicolonToken) {
			continue
		}
		if p.token == ast.KindExportKeyword {
			// `pub fn`/`pub const` declarations are not enum members.
			p.nextToken()
			continue
		}
		if p.zigIsIdent("comptime") || p.zigIsIdent("test") {
			p.nextToken()
			if p.token == ast.KindOpenBraceToken {
				p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
			} else {
				p.zigSkipToSemicolon()
			}
			continue
		}
		if p.token == ast.KindFunctionKeyword {
			p.zigParseContainerMethod(p.nodePos(), false)
			continue
		}
		if p.token == ast.KindConstKeyword || p.token == ast.KindVarKeyword {
			p.zigParseContainerBinding(p.nodePos(), false)
			continue
		}
		if p.token == ast.KindAtToken {
			// `@"name"` enum member.
			p.nextToken()
			if p.token == ast.KindStringLiteral {
				members = append(members, p.scanner.TokenValue())
				p.nextToken()
			}
			continue
		}
		if !tokenIsIdentifierOrKeyword(p.token) && p.token != ast.KindStringLiteral {
			p.nextToken()
			continue
		}
		members = append(members, p.scanner.TokenValue())
		p.nextToken()
		if p.parseOptional(ast.KindEqualsToken) {
			p.zigSkipTypeUntil(ast.KindCommaToken, ast.KindCloseBraceToken)
		}
	}
	return members
}

// zigParseContainerMembers parses the body of a struct/union into class/interface/namespace members.
func (p *Parser) zigParseContainerMembers() []*ast.Node {
	var members []*ast.Node
	for p.token != ast.KindCloseBraceToken && p.token != ast.KindEndOfFile {
		if p.parseOptional(ast.KindCommaToken) || p.parseOptional(ast.KindSemicolonToken) {
			continue
		}
		memberPos := p.nodePos()
		if p.zigIsIdent("comptime") || p.zigIsIdent("test") || p.zigIsIdent("usingnamespace") {
			p.nextToken()
			if p.token == ast.KindOpenBraceToken {
				p.zigSkipBalanced(ast.KindOpenBraceToken, ast.KindCloseBraceToken)
			} else {
				p.zigSkipToSemicolon()
			}
			continue
		}
		exported := false
		if p.token == ast.KindExportKeyword {
			exported = true
			p.nextToken()
		}
		before := p.scanner.TokenFullStart()
		switch {
		case p.token == ast.KindFunctionKeyword:
			if method := p.zigParseContainerMethod(memberPos, exported); method != nil {
				members = append(members, method)
			}
		case p.token == ast.KindConstKeyword || p.token == ast.KindVarKeyword:
			members = append(members, p.zigParseContainerBinding(memberPos, exported)...)
		case tokenIsIdentifierOrKeyword(p.token) || p.token == ast.KindStringLiteral:
			if field := p.zigParseContainerField(memberPos); field != nil {
				members = append(members, field)
			}
		default:
			p.nextToken()
		}
		if p.scanner.TokenFullStart() == before {
			p.nextToken()
		}
	}
	return members
}

// zigParseContainerField parses `name: Type [= default]` as a class property.
func (p *Parser) zigParseContainerField(pos int) *ast.Node {
	name := p.parsePropertyName()
	if p.token != ast.KindColonToken {
		p.zigSkipTypeUntil(ast.KindCommaToken, ast.KindCloseBraceToken)
		return nil
	}
	p.nextToken()
	fieldType := p.zigTryParseType(pos, func() bool {
		return p.token == ast.KindEqualsToken || p.token == ast.KindCommaToken || p.token == ast.KindCloseBraceToken
	})
	if fieldType == nil {
		p.zigSkipTypeUntil(ast.KindEqualsToken, ast.KindCommaToken, ast.KindCloseBraceToken)
		fieldType = p.zigUnknownTypeAt(pos)
	}
	if p.parseOptional(ast.KindEqualsToken) {
		p.zigSkipTypeUntil(ast.KindCommaToken, ast.KindCloseBraceToken)
	}
	postfix := p.factory.NewToken(ast.KindExclamationToken)
	postfix.Loc = core.NewTextRange(pos, pos)
	return p.finishNode(p.factory.NewPropertyDeclaration(nil, name, postfix, fieldType, nil), pos)
}

// zigParseContainerMethod parses a container function and lowers it to a function declaration whose
// body is synthesized, so `zigMethodSignature` can derive the interface signature.
func (p *Parser) zigParseContainerMethod(pos int, exported bool) *ast.Node {
	p.nextToken() // `fn`
	name := p.factory.NewIdentifier("anonymous")
	if p.token == ast.KindIdentifier {
		name = p.parseIdentifier()
	} else if tokenIsIdentifierOrKeyword(p.token) || p.token == ast.KindStringLiteral {
		text := p.scanner.TokenValue()
		if text == "" {
			text = "fn"
		}
		p.nextToken()
		name = p.newIdentifierAt("_"+text+"_"+strconv.Itoa(pos), core.NewTextRange(pos, pos))
	}
	params := p.zigParseFunctionParameters()
	p.zigSkipPostParamModifiers()
	returnType := p.zigParseReturnType(pos)
	body := p.zigSynthesizeFunctionBody(returnType, p.nodePos())
	return p.finishNode(p.factory.NewFunctionDeclaration(
		p.zigExportModifiers(exported, pos), nil, name, nil,
		p.newNodeList(core.NewTextRange(pos, p.nodePos()), params),
		returnType, nil, body,
	), pos)
}

// zigParseContainerBinding parses a nested `const`/`var` declaration inside a container.
func (p *Parser) zigParseContainerBinding(pos int, exported bool) []*ast.Node {
	isConst := p.token == ast.KindConstKeyword
	p.nextToken()
	if !tokenIsIdentifierOrKeyword(p.token) {
		p.zigSkipToSemicolon()
		return nil
	}
	nameIsKeyword := p.token != ast.KindIdentifier
	name := p.parseIdentifierName()
	if nameIsKeyword {
		name = p.newIdentifierAt("_"+name.Text()+"_"+strconv.Itoa(pos), name.Loc)
	}
	declaredType := (*ast.Node)(nil)
	if p.token == ast.KindColonToken {
		p.nextToken()
		declaredType = p.zigTryParseType(pos, func() bool {
			return p.token == ast.KindEqualsToken || p.token == ast.KindSemicolonToken || p.token == ast.KindCommaToken || p.token == ast.KindCloseBraceToken
		})
	}
	if p.token == ast.KindEqualsToken {
		p.nextToken()
		if spec, specLoc, ok := p.zigTryParseImportExpression(); ok {
			p.parseOptional(ast.KindSemicolonToken)
			return p.zigHoistContainerImport(name, spec, exported, pos, specLoc)
		}
		if container := p.zigTryParseContainer(name, exported, pos); container != nil {
			return container
		}
		if alias := p.zigTryParseQualifiedAlias(name, exported, pos, true); alias != nil {
			return alias
		}
		p.zigSkipValue()
	} else {
		p.zigSkipToSemicolon()
	}

	valueType := declaredType
	if valueType == nil {
		valueType = p.zigUnknownTypeAt(pos)
	}
	end := name.End()
	decl := p.finishNodeWithEnd(p.factory.NewVariableDeclaration(
		p.newIdentifierLike(name), nil, valueType, p.zigUnknownValueOfType(valueType, pos),
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
	if declaredType != nil {
		return []*ast.Node{valueStatement}
	}
	typeAlias := p.finishNodeWithEnd(p.factory.NewTypeAliasDeclaration(
		p.zigExportModifiers(exported, pos),
		p.newIdentifierAt(zigTypeAliasName(name.Text()), name.Loc),
		nil,
		p.zigUnknownTypeAt(pos),
	), pos, name.End())
	return []*ast.Node{typeAlias, valueStatement}
}

// zigHoistContainerImport turns a container-level `const X = @import("spec")` into a module-scope
// import (ES imports cannot appear in namespaces). The import stays visible inside the container, so
// field/method types that reference `X` keep resolving.
func (p *Parser) zigHoistContainerImport(name *ast.Node, spec string, exported bool, pos int, specLoc core.TextRange) []*ast.Node {
	p.zigHoistedImports = append(p.zigHoistedImports, p.zigImportDeclaration(name.Text(), spec, pos, specLoc))
	return nil
}
