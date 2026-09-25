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
		// The contextual type may start before the literal (e.g. the `Foo` in `Foo{}`), so extend the
		// assertion's start to contain it and keep node ranges well formed for the language service.
		start := pos
		if typeNode.Pos() < start {
			start = typeNode.Pos()
		}
		return p.finishNode(p.factory.NewAsExpression(result, typeNode), start)
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
	switch {
	case p.token == ast.KindIdentifier && p.lookAhead((*Parser).nextTokenIsColon):
		member = p.parseZigContainerField()
	case p.zigContainerMemberIsConstVar():
		member = p.parseZigContainerBinding()
	default:
		member = p.parseStatement()
	}
	// Real Zig separates container members with commas; also accept a semicolon for the dialect.
	p.parseOptional(ast.KindCommaToken)
	return member
}

// zigContainerMemberIsConstVar reports whether the upcoming member is a `[pub] const/var` binding.
func (p *Parser) zigContainerMemberIsConstVar() bool {
	return p.lookAhead(func(p *Parser) bool {
		if p.token == ast.KindExportKeyword {
			p.nextToken()
		}
		return p.token == ast.KindConstKeyword || p.token == ast.KindVarKeyword
	})
}

// parseZigContainerBinding parses a single `[pub] const/var` container member. Unlike the generic
// statement parser it does not consume the comma-separated declaration list, because in a Zig
// container the comma is a member separator.
func (p *Parser) parseZigContainerBinding() *ast.Node {
	pos := p.nodePos()
	jsdoc := p.jsdocScannerInfo()
	modifiers := p.parseModifiers()
	isConst := p.token == ast.KindConstKeyword
	p.nextToken() // const / var
	decl := p.parseVariableDeclaration()
	listFlags := ast.NodeFlagsLet
	if isConst {
		listFlags = ast.NodeFlagsConst
	}
	declList := p.finishNode(p.factory.NewVariableDeclarationList(p.newNodeList(decl.Loc, []*ast.Node{decl}), listFlags), pos)
	result := p.finishNode(p.factory.NewVariableStatement(modifiers, declList), pos)
	p.withJSDoc(result, jsdoc)
	p.checkJSSyntax(result)
	return result
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
	// Zig's `undefined` initializer means "leave uninitialized"; drop it so the field is treated as
	// definitely assigned instead of producing a `undefined` is not assignable` error.
	if p.zigIsUndefinedExpression(initializer) {
		initializer = nil
	}
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
	if len(p.zigStructExprs) == 0 && len(p.zigEnumExprs) == 0 && len(p.zigImportExprs) == 0 {
		return statements
	}

	// Discover `fn F() type { return struct { ... }; }` factories in this scope.
	factories := map[string]*zigTypeFactory{}
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
			factories[name.Text()] = &zigTypeFactory{fn: stmt, ret: ret}
		}
	}

	consumed := map[*ast.Node]bool{}
	changed := false
	out := make([]*ast.Node, 0, len(statements))
	for _, stmt := range statements {
		if p.zigHoistImportBinding(stmt) {
			changed = true
			continue
		}
		if expanded, ok := p.expandZigStruct(stmt, factories, consumed); ok {
			out = append(out, expanded...)
			changed = true
		} else {
			out = append(out, stmt)
		}
	}
	// A factory whose container was never inlined would leave property declarations inside a module
	// block, which is an invalid tree shape. Replace those returns with an equivalent class expression.
	for _, factory := range factories {
		ret := factory.ret
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
// zigTypeFactory is a discovered `fn F(...) type { return struct { ... }; }` declaration.
type zigTypeFactory struct {
	fn  *ast.Node
	ret *ast.Node
}

func (p *Parser) expandZigStruct(stmt *ast.Node, factories map[string]*zigTypeFactory, consumed map[*ast.Node]bool) ([]*ast.Node, bool) {
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

	// Case 0: the initializer is an enum container, e.g. `const Color = enum { red, green };`.
	if init.Kind == ast.KindModuleExpression {
		if members, ok := p.zigEnumExprs[init]; ok {
			return p.zigEnumDeclarations(nameNode, members, exported, start, end), true
		}
	}

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
			factory, ok := factories[callee.Text()]
			if !ok || consumed[factory.ret] {
				return nil, false
			}
			structExpr := factory.ret.AsReturnStatement().Expression
			if structExpr == nil || structExpr.Kind != ast.KindModuleExpression {
				return nil, false
			}
			body := structExpr.AsModuleExpression().Body
			if body == nil || body.Kind != ast.KindModuleBlock {
				return nil, false
			}
			consumed[factory.ret] = true
			// Instantiate the factory: substitute its type parameters with the call arguments and
			// rewrite self-references to the synthesized type name.
			p.zigSubstituteFactoryTypes(body, factory, init.AsCallExpression(), nameNode.Text())
			// The factory now returns the synthesized type's value, so the anonymous container
			// never reaches the binder. Reuse the call-site name's location so the printer emits the
			// synthesized type name rather than the original `struct { ... }` source text.
			factory.ret.AsReturnStatement().Expression = p.newIdentifierAt(nameNode.Text(), nameNode.Loc)
			p.overrideParentInImmediateChildren(factory.ret)
			return p.zigContainerDeclarations(nameNode, body, exported, start, end), true
		}
	}
	return nil, false
}

// zigSubstituteFactoryTypes rewrites the body of an inlined type factory: each factory parameter is
// replaced by the matching call argument (when used as a type), and references to the factory name
// are rewritten to the synthesized type name. This makes `MakeArray(i32)` produce a concrete
// `IntArray` instead of leaking the generic parameter `T` and the factory name.
func (p *Parser) zigSubstituteFactoryTypes(body *ast.Node, factory *zigTypeFactory, call *ast.CallExpression, newName string) {
	fnDecl := factory.fn.AsFunctionDeclaration()
	paramTypes := map[string]*ast.Node{}
	if fnDecl != nil && fnDecl.Parameters != nil && call != nil && call.Arguments != nil {
		for i, param := range fnDecl.Parameters.Nodes {
			if i >= len(call.Arguments.Nodes) {
				break
			}
			paramDecl := param.AsParameterDeclaration()
			if paramDecl == nil {
				continue
			}
			paramName := paramDecl.Name()
			if paramName == nil || paramName.Kind != ast.KindIdentifier {
				continue
			}
			if argType := p.zigExpressionToTypeNode(call.Arguments.Nodes[i]); argType != nil {
				paramTypes[paramName.Text()] = argType
			}
		}
	}
	factoryName := ""
	if fnDecl != nil && fnDecl.Name() != nil {
		factoryName = fnDecl.Name().Text()
	}
	for _, member := range body.AsModuleBlock().Statements.Nodes {
		p.zigSubstituteFactoryMember(member, paramTypes, factoryName, newName)
	}
}

// zigSubstituteFactoryMember applies factory substitution to the type annotations of one member.
func (p *Parser) zigSubstituteFactoryMember(member *ast.Node, paramTypes map[string]*ast.Node, factoryName, newName string) {
	switch member.Kind {
	case ast.KindPropertyDeclaration:
		property := member.AsPropertyDeclaration()
		property.Type = p.zigSubstituteFactoryTypeNode(property.Type, paramTypes, factoryName, newName)
	case ast.KindFunctionDeclaration:
		function := member.AsFunctionDeclaration()
		if function.Parameters != nil {
			for _, param := range function.Parameters.Nodes {
				paramDecl := param.AsParameterDeclaration()
				if paramDecl != nil {
					paramDecl.Type = p.zigSubstituteFactoryTypeNode(paramDecl.Type, paramTypes, factoryName, newName)
				}
			}
		}
		function.Type = p.zigSubstituteFactoryTypeNode(function.Type, paramTypes, factoryName, newName)
	}
	// Rewritten/new children need their parent pointers restored for the binder.
	p.zigFixParents(member, member.Parent)
}

// zigFixParents recursively restores parent pointers after a subtree was rewritten.
func (p *Parser) zigFixParents(node *ast.Node, parent *ast.Node) {
	if node == nil {
		return
	}
	node.Parent = parent
	node.ForEachChild(func(child *ast.Node) bool {
		p.zigFixParents(child, node)
		return false
	})
}

// zigSubstituteFactoryTypeNode rewrites a single type node for factory instantiation.
func (p *Parser) zigSubstituteFactoryTypeNode(node *ast.Node, paramTypes map[string]*ast.Node, factoryName, newName string) *ast.Node {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case ast.KindTypeReference:
		reference := node.AsTypeReferenceNode()
		if reference.TypeName == nil || reference.TypeName.Kind != ast.KindIdentifier {
			return node
		}
		name := reference.TypeName.Text()
		if replacement, ok := paramTypes[name]; ok {
			return p.factory.DeepCloneReparse(replacement)
		}
		if name == factoryName && factoryName != newName {
			// Use a synthesized location so the printer emits the new text instead of the source text.
			reference.TypeName = p.newIdentifierAt(newName, core.NewTextRange(-1, -1))
			reference.TypeArguments = nil
		}
		return node
	case ast.KindArrayType:
		array := node.AsArrayTypeNode()
		array.ElementType = p.zigSubstituteFactoryTypeNode(array.ElementType, paramTypes, factoryName, newName)
		return node
	case ast.KindUnionType:
		union := node.AsUnionTypeNode()
		if union.Types != nil {
			for i, member := range union.Types.Nodes {
				union.Types.Nodes[i] = p.zigSubstituteFactoryTypeNode(member, paramTypes, factoryName, newName)
			}
		}
		return node
	}
	return node
}

// zigContainerDeclarations lowers a container body to a `type X = { ... }` alias plus a TC39 module
// declaration (`module X { ... }`). Fields and `self` methods describe the instance type; the
// remaining declarations (functions, nested containers, imports) live in the module, which is what
// Zig containers actually are (a type and a namespace at the same time).
func (p *Parser) zigContainerDeclarations(nameNode *ast.Node, body *ast.Node, exported bool, start, end int) []*ast.Node {
	// Nested containers and enums are desugared before the members are classified.
	bodyMembers := p.desugarZigStructs(body.AsModuleBlock().Statements.Nodes)
	var typeMembers, moduleMembers []*ast.Node
	for _, member := range bodyMembers {
		if member.Kind == ast.KindPropertyDeclaration {
			property := member.AsPropertyDeclaration()
			typeMembers = append(typeMembers, p.finishNodeWithEnd(p.factory.NewPropertySignatureDeclaration(
				nil, property.Name(), nil, property.Type, nil,
			), member.Pos(), member.End()))
			continue
		}
		moduleMembers = append(moduleMembers, member)
		if member.Kind == ast.KindFunctionDeclaration {
			if sig, ok := p.zigMethodSignature(member); ok {
				typeMembers = append(typeMembers, sig)
			}
		}
	}

	memberLoc := body.AsModuleBlock().Statements.Loc
	typeAlias := p.finishNodeWithEnd(p.factory.NewTypeAliasDeclaration(
		p.zigExportModifiers(exported, start),
		p.newIdentifierLike(nameNode),
		nil,
		p.finishNodeWithEnd(p.factory.NewTypeLiteralNode(p.newNodeList(memberLoc, typeMembers)), start, end),
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

	return []*ast.Node{typeAlias, moduleDecl}
}

// zigEnumDeclarations builds the `type X = "a" | "b"` alias and the `namespace X` value namespace
// for a Zig enum. Enum literals already lower to string literals, so the members are emitted as
// exported string constants.
func (p *Parser) zigEnumDeclarations(nameNode *ast.Node, members []string, exported bool, start, end int) []*ast.Node {
	loc := core.NewTextRange(start, end)

	synthesizedLoc := core.NewTextRange(-1, -1)
	var typeNodes []*ast.Node
	var valueMembers []*ast.Node
	for _, member := range members {
		stringLiteral := p.factory.NewStringLiteral(member, ast.TokenFlagsNone)
		stringLiteral.Loc = synthesizedLoc
		typeNodes = append(typeNodes, p.finishNodeWithEnd(p.factory.NewLiteralTypeNode(stringLiteral), start, end))

		value := p.factory.NewStringLiteral(member, ast.TokenFlagsNone)
		value.Loc = synthesizedLoc
		decl := p.finishNodeWithEnd(p.factory.NewVariableDeclaration(p.newIdentifierAt(member, synthesizedLoc), nil, nil, value), start, end)
		declList := p.finishNodeWithEnd(p.factory.NewVariableDeclarationList(p.newNodeList(loc, []*ast.Node{decl}), ast.NodeFlagsConst), start, end)
		valueMembers = append(valueMembers, p.finishNodeWithEnd(p.factory.NewVariableStatement(p.zigExportModifiers(true, start), declList), start, end))
	}

	var typeNode *ast.Node
	switch len(typeNodes) {
	case 0:
		typeNode = p.finishNodeWithEnd(p.factory.NewKeywordTypeNode(ast.KindNeverKeyword), start, end)
	case 1:
		typeNode = typeNodes[0]
	default:
		typeNode = p.finishNodeWithEnd(p.factory.NewUnionTypeNode(p.newNodeList(loc, typeNodes)), start, end)
	}
	typeAlias := p.finishNodeWithEnd(p.factory.NewTypeAliasDeclaration(
		p.zigExportModifiers(exported, start),
		p.newIdentifierLike(nameNode),
		nil,
		typeNode,
	), start, end)

	moduleBlock := p.finishNodeWithEnd(p.factory.NewModuleBlock(p.newNodeList(loc, valueMembers)), start, end)
	moduleDecl := p.finishNodeWithEnd(p.factory.NewModuleDeclaration(
		p.zigExportModifiers(exported, start),
		ast.KindModuleKeyword,
		p.newIdentifierLike(nameNode),
		nil,
		moduleBlock,
	), start, end)
	moduleDecl.Flags |= ast.NodeFlagsModuleFragment

	return []*ast.Node{typeAlias, moduleDecl}
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
