package zig_parser

import (
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// This file lowers Zig's `@builtin(...)` expressions. Builtins whose runtime semantics matter are
// translated to equivalent TypeScript; the rest degrade to a permissive `globalThis` access so the
// enclosing declarations keep their real types instead of falling back to `any`.

// parseZigBuiltinExpression parses `@name(args...)` and lowers it to a TypeScript expression.
func (p *Parser) parseZigBuiltinExpression() *ast.Expression {
	pos := p.nodePos()
	p.parseExpected(ast.KindAtToken)
	name := ""
	if p.token == ast.KindIdentifier || tokenIsIdentifierOrKeyword(p.token) {
		name = p.scanner.TokenValue()
		p.nextToken()
	}
	var args []*ast.Node
	if p.token == ast.KindOpenParenToken {
		args = p.parseArgumentList().Nodes
	}

	switch name {
	case "import":
		// A `const X = @import("spec")` binding is hoisted to a real module import by the desugar
		// pass. Any other use has no module to bind to and degrades to a permissive value.
		expression := p.zigGlobalThisAny(pos)
		spec := ""
		if len(args) > 0 && (args[0].Kind == ast.KindStringLiteral || args[0].Kind == ast.KindNoSubstitutionTemplateLiteral) {
			spec = args[0].Text()
		}
		if p.zigImportExprs == nil {
			p.zigImportExprs = make(map[*ast.Node]string)
		}
		p.zigImportExprs[expression] = spec
		return expression
	case "as":
		if len(args) == 2 {
			return args[1]
		}
	case "hasDecl":
		if len(args) == 2 {
			operator := p.factory.NewToken(ast.KindInKeyword)
			operator.Loc = core.NewTextRange(-1, -1)
			right := p.finishNodeWithEnd(p.factory.NewAsExpression(args[0], p.zigAnyKeywordType()), -1, -1)
			binary := p.factory.NewBinaryExpression(nil, args[1], nil, operator, p.wrapZigParen(right))
			return p.finishNodeWithEnd(binary, pos, p.nodePos())
		}
	case "sizeOf", "bitSizeOf", "alignOf":
		return p.finishNodeWithEnd(p.factory.NewNumericLiteral("0", ast.TokenFlagsNone), pos, p.nodePos())
	case "typeName":
		return p.finishNodeWithEnd(p.factory.NewStringLiteral("", ast.TokenFlagsNone), pos, p.nodePos())
	}
	// `@import` and every other builtin (in value position) have no direct equivalent. A
	// `globalThis` access is a valid expression of type `any` and never leaves an unresolved name.
	return p.zigGlobalThisAny(pos)
}

// zigAnyKeywordType synthesizes an `any` keyword type.
func (p *Parser) zigAnyKeywordType() *ast.Node {
	node := p.factory.NewKeywordTypeNode(ast.KindAnyKeyword)
	node.Loc = core.NewTextRange(-1, -1)
	return node
}

// zigGlobalThisAny builds `(globalThis as any)`.
func (p *Parser) zigGlobalThisAny(pos int) *ast.Expression {
	globalThis := p.factory.NewIdentifier("globalThis")
	globalThis.Loc = core.NewTextRange(-1, -1)
	asExpression := p.finishNodeWithEnd(p.factory.NewAsExpression(globalThis, p.zigAnyKeywordType()), -1, -1)
	return p.finishNodeWithEnd(p.factory.NewParenthesizedExpression(asExpression), pos, p.nodePos())
}

// wrapZigParen parenthesizes expr, which is used when a lowered builtin argument needs grouping.
func (p *Parser) wrapZigParen(expr *ast.Node) *ast.Node {
	return p.finishNodeWithEnd(p.factory.NewParenthesizedExpression(expr), -1, -1)
}

// zigHoistImportBinding recognizes `const Name = @import("spec")` and queues a module-scope
// declaration for it. Imports are hoisted to the top of the file because Zig container members are
// visible to the container's fields, which the class/namespace desugaring splits across scopes.
func (p *Parser) zigHoistImportBinding(stmt *ast.Node) bool {
	if stmt.Kind != ast.KindVariableStatement {
		return false
	}
	vs := stmt.AsVariableStatement()
	if vs.DeclarationList == nil || vs.DeclarationList.Kind != ast.KindVariableDeclarationList {
		return false
	}
	decls := vs.DeclarationList.AsVariableDeclarationList().Declarations
	if decls == nil || len(decls.Nodes) != 1 {
		return false
	}
	decl := decls.Nodes[0].AsVariableDeclaration()
	if decl == nil || decl.Initializer == nil {
		return false
	}
	spec, ok := p.zigImportExprs[decl.Initializer]
	if !ok {
		return false
	}
	nameNode := decl.Name()
	if nameNode == nil || nameNode.Kind != ast.KindIdentifier {
		return false
	}
	p.zigHoistedImports = append(p.zigHoistedImports, p.zigImportDeclaration(nameNode.Text(), spec, stmt.Pos()))
	return true
}

// zigImportDeclaration synthesizes either `import * as Name from "./spec.zig"` or, for the
// non-module `builtin`/`root` specifiers, a permissive module-scope `const`.
func (p *Parser) zigImportDeclaration(name, spec string, pos int) *ast.Node {
	nameIdentifier := p.factory.NewIdentifier(name)
	nameIdentifier.Loc = core.NewTextRange(-1, -1)
	if spec == "" || spec == "builtin" || spec == "root" {
		globalThis := p.factory.NewIdentifier("globalThis")
		globalThis.Loc = core.NewTextRange(-1, -1)
		initializer := p.finishNodeWithEnd(p.factory.NewAsExpression(globalThis, p.zigAnyKeywordType()), -1, -1)
		decl := p.finishNodeWithEnd(p.factory.NewVariableDeclaration(nameIdentifier, nil, nil, initializer), pos, pos)
		declList := p.finishNodeWithEnd(p.factory.NewVariableDeclarationList(p.newNodeList(core.NewTextRange(pos, pos), []*ast.Node{decl}), ast.NodeFlagsConst), pos, pos)
		return p.finishNodeWithEnd(p.factory.NewVariableStatement(nil, declList), pos, pos)
	}
	moduleSpecifier := spec
	if strings.HasSuffix(spec, ".zig") && !strings.HasPrefix(spec, ".") && !strings.HasPrefix(spec, "/") {
		moduleSpecifier = "./" + spec
	}
	specLiteral := p.factory.NewStringLiteral(moduleSpecifier, ast.TokenFlagsNone)
	specLiteral.Loc = core.NewTextRange(-1, -1)
	// Resolve the import to the module's default export: a Zig file is itself a container and
	// default-exports its `Self`, so `const X = @import("y.zig")` binds `X` to that type/value.
	importClause := p.finishNodeWithEnd(p.factory.NewImportClause(ast.KindUnknown, nameIdentifier, nil), pos, pos)
	return p.finishNodeWithEnd(p.factory.NewImportDeclaration(nil, importClause, specLiteral, nil), pos, pos)
}
