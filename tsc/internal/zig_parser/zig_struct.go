package zig_parser

import (
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// nextTokenIsColon is a lookAhead callback used to recognize container field declarations.
func (p *Parser) nextTokenIsColon() bool {
	return p.nextToken() == ast.KindColonToken
}

// parseZigDotExpression parses Zig's leading-dot expression forms: `.{ ... }` container/array
// literals and `.name` enum literals.
func (p *Parser) parseZigDotExpression() *ast.Expression {
	pos := p.nodePos()
	p.parseExpected(ast.KindDotToken)
	if p.token == ast.KindOpenBraceToken {
		return p.parseZigContainerLiteral(pos)
	}
	name := p.parseIdentifierName()
	// `.name` is an enum literal. When the surrounding declaration has a known type, qualify it so a
	// real (synthesized) enum resolves; otherwise fall back to a string literal.
	if p.zigContextualType != nil && p.zigContextualType.Kind == ast.KindTypeReference {
		if receiver := p.entityNameToExpression(p.zigContextualType.AsTypeReferenceNode().TypeName); receiver != nil {
			return p.finishNode(p.factory.NewPropertyAccessExpression(receiver, nil, name, ast.NodeFlagsNone), pos)
		}
	}
	return p.finishNode(p.factory.NewStringLiteral(name.Text(), ast.TokenFlagsNone), pos)
}

// entityNameToExpression converts a (qualified) type name such as `Shape.Kind` into the equivalent
// left-hand-side expression so an enum literal can be written as `Shape.Kind.member`.
func (p *Parser) entityNameToExpression(name *ast.Node) *ast.Expression {
	if name == nil {
		return nil
	}
	switch name.Kind {
	case ast.KindIdentifier:
		return p.newIdentifierLike(name)
	case ast.KindQualifiedName:
		qualified := name.AsQualifiedName()
		left := p.entityNameToExpression(qualified.Left)
		if left == nil {
			return nil
		}
		return p.finishNode(p.factory.NewPropertyAccessExpression(left, nil, p.newIdentifierLike(qualified.Right), ast.NodeFlagsNone), name.Pos())
	}
	return nil
}

// parseZigContainerLiteral parses `.{ ... }`. Elements written as `.field = value` produce an object
// literal; bare elements produce an array literal. When the declaration has a contextual type the
// literal is coerced to it, mirroring Zig's "fill in the defaults" semantics.
func (p *Parser) parseZigContainerLiteral(pos int) *ast.Expression {
	openBracePosition := p.scanner.TokenStart()
	openBraceParsed := p.parseExpected(ast.KindOpenBraceToken)
	multiLine := p.hasPrecedingLineBreak()
	var elements []*ast.Node
	isObject := false
	for p.token != ast.KindCloseBraceToken && p.token != ast.KindEndOfFile {
		if p.parseOptional(ast.KindCommaToken) {
			continue
		}
		before := p.nodePos()
		element := p.parseZigLiteralElement()
		if element != nil {
			elements = append(elements, element)
			if element.Kind == ast.KindPropertyAssignment || element.Kind == ast.KindShorthandPropertyAssignment {
				isObject = true
			}
		}
		p.parseOptional(ast.KindCommaToken)
		if p.nodePos() == before {
			p.nextToken()
		}
	}
	p.parseExpectedMatchingBrackets(ast.KindOpenBraceToken, ast.KindCloseBraceToken, openBraceParsed, openBracePosition)

	loc := core.NewTextRange(pos, p.nodePos())
	var result *ast.Node
	if isObject || len(elements) == 0 {
		result = p.finishNode(p.factory.NewObjectLiteralExpression(p.newNodeList(loc, elements), multiLine), pos)
	} else {
		result = p.finishNode(p.factory.NewArrayLiteralExpression(p.newNodeList(loc, elements), multiLine), pos)
	}
	if p.zigContextualType != nil {
		typeNode := p.factory.DeepCloneReparse(p.zigContextualType)
		return p.finishNode(p.factory.NewAsExpression(result, typeNode), pos)
	}
	return result
}

// parseZigLiteralElement parses one element of a `.{ ... }` literal.
func (p *Parser) parseZigLiteralElement() *ast.Node {
	if p.token == ast.KindDotToken && p.lookAhead((*Parser).zigLiteralIsFieldAssignment) {
		pos := p.nodePos()
		p.nextToken() // '.'
		name := p.parseIdentifierName()
		p.nextToken() // '='
		value := p.parseAssignmentExpressionOrHigher()
		return p.finishNode(p.factory.NewPropertyAssignment(nil, name, nil, nil, value), pos)
	}
	return p.parseAssignmentExpressionOrHigher()
}

// zigLiteralIsFieldAssignment reports whether the upcoming `.<name> =` starts a field initializer.
func (p *Parser) zigLiteralIsFieldAssignment() bool {
	return p.nextToken() == ast.KindIdentifier && p.nextToken() == ast.KindEqualsToken
}

// parseZigContainerMember parses a single member of a Zig container body. Fields
// (`name: Type = value;`) are parsed as property declarations so that they carry proper type
// information; every other member is an ordinary statement (function/const/var/enum declaration)
// that stays in the container's module body.
func (p *Parser) parseZigContainerMember() *ast.Node {
	var member *ast.Node
	if p.token == ast.KindIdentifier && p.lookAhead((*Parser).nextTokenIsColon) {
		member = p.parseZigContainerField()
	} else {
		member = p.parseStatement()
	}
	// Real Zig separates container members with commas; also accept a semicolon for the dialect.
	p.parseOptional(ast.KindCommaToken)
	return member
}

// parseZigContainerField parses a container field (`name: Type` with an optional `= default`). It
// does not require a trailing semicolon, since Zig separates members with commas.
func (p *Parser) parseZigContainerField() *ast.Node {
	pos := p.nodePos()
	jsdoc := p.jsdocScannerInfo()
	name := p.parsePropertyName()
	typeNode := p.parseTypeAnnotation()
	savedContextualType := p.zigContextualType
	p.zigContextualType = typeNode
	initializer := p.doInContext(ast.NodeFlagsYieldContext|ast.NodeFlagsAwaitContext|ast.NodeFlagsDisallowInContext, false, (*Parser).parseInitializer)
	p.zigContextualType = savedContextualType
	// Zig fields are always assigned when a value is constructed, so a field without a default value
	// is definitely assigned. Mark it with `!` so `strictPropertyInitialization` is happy.
	var postfixToken *ast.Node
	if initializer == nil {
		postfixToken = p.factory.NewToken(ast.KindExclamationToken)
		postfixToken.Loc = core.NewTextRange(p.nodePos(), p.nodePos())
	}
	result := p.finishNode(p.factory.NewPropertyDeclaration(nil, name, postfixToken, typeNode, initializer), pos)
	p.withJSDoc(result, jsdoc)
	p.checkJSSyntax(result)
	return result
}

// desugarZigStructs rewrites `const X = struct { ... }` (and nested equivalents) into the
// class/interface/module declaration triple that models a Zig container. It also inlines Zig
// "type factories": a `const X = F()` whose `F` is a `type`-returning function whose body returns a
// container literal is modeled as the same triple named `X`. Doing this on real AST nodes (rather
// than rewriting text) keeps source positions intact for the language service.
func (p *Parser) desugarZigStructs(statements []*ast.Node) []*ast.Node {
	if len(p.zigStructExprs) == 0 {
		return statements
	}

	// Discover `fn F() type { return struct { ... }; }` factories in this scope.
	factories := map[string]*ast.Node{}
	for _, stmt := range statements {
		if stmt.Kind != ast.KindFunctionDeclaration {
			continue
		}
		fnDecl := stmt.AsFunctionDeclaration()
		name := fnDecl.Name()
		if name == nil || name.Kind != ast.KindIdentifier {
			continue
		}
		if ret := p.zigTypeFactoryReturn(stmt); ret != nil {
			factories[name.Text()] = ret
		}
	}

	consumed := map[*ast.Node]bool{}
	changed := false
	out := make([]*ast.Node, 0, len(statements))
	for _, stmt := range statements {
		if expanded, ok := p.expandZigStruct(stmt, factories, consumed); ok {
			out = append(out, expanded...)
			changed = true
		} else {
			out = append(out, stmt)
		}
	}
	// A factory whose container was never inlined would leave property declarations inside a module
	// block, which is an invalid tree shape. Replace those returns with an equivalent class expression.
	for _, ret := range factories {
		if consumed[ret] {
			continue
		}
		ret.AsReturnStatement().Expression = p.zigClassExpression(ret.AsReturnStatement().Expression)
		p.overrideParentInImmediateChildren(ret)
		changed = true
	}
	if !changed {
		return statements
	}
	return out
}

// expandZigStruct expands a single `const X = struct { ... }` variable statement, or a
// `const X = F()` call to an in-scope type factory. It returns false when the statement is neither.
func (p *Parser) expandZigStruct(stmt *ast.Node, factories map[string]*ast.Node, consumed map[*ast.Node]bool) ([]*ast.Node, bool) {
	if stmt.Kind != ast.KindVariableStatement {
		return nil, false
	}
	vs := stmt.AsVariableStatement()
	if vs.DeclarationList == nil || vs.DeclarationList.Kind != ast.KindVariableDeclarationList {
		return nil, false
	}
	decls := vs.DeclarationList.AsVariableDeclarationList().Declarations
	if decls == nil || len(decls.Nodes) != 1 {
		return nil, false
	}
	decl := decls.Nodes[0].AsVariableDeclaration()
	if decl == nil {
		return nil, false
	}
	nameNode := decl.Name()
	if nameNode == nil || nameNode.Kind != ast.KindIdentifier {
		return nil, false
	}
	init := decl.Initializer
	if init == nil {
		return nil, false
	}

	start, end := stmt.Pos(), stmt.End()
	exported := ast.HasSyntacticModifier(stmt, ast.ModifierFlagsExport)

	// Case 1: the initializer is a container literal, e.g. `const Counter = struct { ... };`.
	if init.Kind == ast.KindModuleExpression && p.zigStructExprs[init] {
		if body := init.AsModuleExpression().Body; body != nil && body.Kind == ast.KindModuleBlock {
			return p.zigContainerDeclarations(nameNode, body, exported, start, end), true
		}
		return nil, false
	}

	// Case 2: the initializer calls a type factory, e.g. `const MyCounter = MakeCounter();`.
	if init.Kind == ast.KindCallExpression {
		callee := init.AsCallExpression().Expression
		if callee != nil && callee.Kind == ast.KindIdentifier {
			ret, ok := factories[callee.Text()]
			if !ok || consumed[ret] {
				return nil, false
			}
			structExpr := ret.AsReturnStatement().Expression
			if structExpr == nil || structExpr.Kind != ast.KindModuleExpression {
				return nil, false
			}
			body := structExpr.AsModuleExpression().Body
			if body == nil || body.Kind != ast.KindModuleBlock {
				return nil, false
			}
			consumed[ret] = true
			// The factory now returns the synthesized type's value, so the anonymous container
			// never reaches the binder.
			ret.AsReturnStatement().Expression = p.newIdentifierAt(nameNode.Text(), structExpr.Loc)
			p.overrideParentInImmediateChildren(ret)
			return p.zigContainerDeclarations(nameNode, body, exported, start, end), true
		}
	}
	return nil, false
}

// zigContainerDeclarations builds the `class X` + `interface X` + `module X` triple for a container
// body. Fields become class properties, `self` methods become interface method signatures, and the
// remaining members stay in the module namespace.
func (p *Parser) zigContainerDeclarations(nameNode *ast.Node, body *ast.Node, exported bool, start, end int) []*ast.Node {
	var classMembers, interfaceMembers, moduleMembers []*ast.Node
	for _, member := range body.AsModuleBlock().Statements.Nodes {
		if member.Kind == ast.KindPropertyDeclaration {
			classMembers = append(classMembers, member)
			continue
		}
		moduleMembers = append(moduleMembers, member)
		if member.Kind == ast.KindFunctionDeclaration {
			if sig, ok := p.zigMethodSignature(member); ok {
				interfaceMembers = append(interfaceMembers, sig)
			}
		}
	}

	memberLoc := body.AsModuleBlock().Statements.Loc
	classDecl := p.finishNodeWithEnd(p.factory.NewClassDeclaration(
		p.zigExportModifiers(exported, start),
		p.newIdentifierLike(nameNode),
		nil, nil,
		p.newNodeList(memberLoc, classMembers),
	), start, end)
	interfaceDecl := p.finishNodeWithEnd(p.factory.NewInterfaceDeclaration(
		p.zigExportModifiers(exported, start),
		p.newIdentifierLike(nameNode),
		nil, nil,
		p.newNodeList(memberLoc, interfaceMembers),
	), start, end)
	moduleBlock := p.finishNodeWithEnd(p.factory.NewModuleBlock(
		p.newNodeList(memberLoc, moduleMembers),
	), body.Pos(), body.End())
	moduleDecl := p.finishNodeWithEnd(p.factory.NewModuleDeclaration(
		p.zigExportModifiers(exported, start),
		ast.KindModuleKeyword,
		p.newIdentifierLike(nameNode),
		nil,
		moduleBlock,
	), start, end)
	moduleDecl.Flags |= ast.NodeFlagsModuleFragment

	return []*ast.Node{classDecl, interfaceDecl, moduleDecl}
}

// zigTypeFactoryReturn returns the `return <container>;` statement for a `fn F(...) type { ... }`
// declaration, or nil when the function does not return a container literal.
func (p *Parser) zigTypeFactoryReturn(fn *ast.Node) *ast.Node {
	fnDecl := fn.AsFunctionDeclaration()
	if fnDecl == nil || fnDecl.Type != nil || fnDecl.Body == nil || fnDecl.Body.Kind != ast.KindBlock {
		return nil
	}
	paramsEnd := 0
	if fnDecl.Parameters != nil {
		paramsEnd = fnDecl.Parameters.End()
	}
	bodyPos := fnDecl.Body.Pos()
	if paramsEnd >= 0 && bodyPos >= paramsEnd && bodyPos <= len(p.sourceText) {
		if !strings.Contains(p.sourceText[paramsEnd:bodyPos], "type") {
			return nil
		}
	}
	for _, stmt := range fnDecl.Body.AsBlock().Statements.Nodes {
		if stmt.Kind != ast.KindReturnStatement {
			continue
		}
		expr := stmt.AsReturnStatement().Expression
		if expr != nil && expr.Kind == ast.KindModuleExpression && p.zigStructExprs[expr] {
			return stmt
		}
	}
	return nil
}

// zigClassExpression lowers a container literal used outside a declaration position to a class
// expression holding its fields, so the resulting tree is always well formed.
func (p *Parser) zigClassExpression(structExpr *ast.Node) *ast.Node {
	if structExpr == nil || structExpr.Kind != ast.KindModuleExpression {
		return structExpr
	}
	body := structExpr.AsModuleExpression().Body
	if body == nil || body.Kind != ast.KindModuleBlock {
		return structExpr
	}
	var members []*ast.Node
	for _, member := range body.AsModuleBlock().Statements.Nodes {
		if member.Kind == ast.KindPropertyDeclaration {
			members = append(members, member)
		}
	}
	return p.finishNodeWithEnd(p.factory.NewClassExpression(
		nil, nil, nil, nil,
		p.newNodeList(body.AsModuleBlock().Statements.Loc, members),
	), structExpr.Pos(), structExpr.End())
}

// zigMethodSignature builds the interface method signature for a container function whose first
// parameter is `self`. The `self` parameter is dropped so the method can be called on an instance.
func (p *Parser) zigMethodSignature(fn *ast.Node) (*ast.Node, bool) {
	fnDecl := fn.AsFunctionDeclaration()
	if fnDecl == nil || fnDecl.Parameters == nil || len(fnDecl.Parameters.Nodes) == 0 {
		return nil, false
	}
	self := fnDecl.Parameters.Nodes[0].AsParameterDeclaration()
	if self == nil {
		return nil, false
	}
	selfName := self.Name()
	if selfName == nil || selfName.Kind != ast.KindIdentifier || selfName.Text() != "self" {
		return nil, false
	}
	funcName := fnDecl.Name()
	if funcName == nil {
		return nil, false
	}
	params := make([]*ast.Node, 0, len(fnDecl.Parameters.Nodes)-1)
	for _, arg := range fnDecl.Parameters.Nodes[1:] {
		params = append(params, p.factory.DeepCloneReparse(arg))
	}
	var returnType *ast.Node
	if fnDecl.Type != nil {
		returnType = p.factory.DeepCloneReparse(fnDecl.Type)
	}
	return p.finishNodeWithEnd(p.factory.NewMethodSignatureDeclaration(
		nil,
		p.newIdentifierLike(funcName),
		nil,
		nil,
		p.newNodeList(fnDecl.Parameters.Loc, params),
		returnType,
	), fn.Pos(), fn.End()), true
}

// zigExportModifiers returns a fresh `export` modifier list for synthesized declarations.
func (p *Parser) zigExportModifiers(exported bool, pos int) *ast.ModifierList {
	if !exported {
		return nil
	}
	mod := p.factory.NewModifier(ast.KindExportKeyword)
	mod.Loc = core.NewTextRange(pos, pos)
	return p.newModifierList(mod.Loc, p.nodeSliceArena.NewSlice1(mod))
}

// newIdentifierLike creates a fresh identifier node that shares the text and position of the given
// node. Synthesized declarations must not share identifier nodes because a node can only have one
// parent in the resulting tree.
func (p *Parser) newIdentifierLike(n *ast.Node) *ast.Node {
	return p.newIdentifierAt(n.Text(), n.Loc)
}

// newIdentifierAt creates a fresh identifier node with the given text and source range.
func (p *Parser) newIdentifierAt(text string, loc core.TextRange) *ast.Node {
	id := p.factory.NewIdentifier(text)
	id.Loc = loc
	return id
}
