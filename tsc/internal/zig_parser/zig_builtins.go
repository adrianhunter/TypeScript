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
	var argsList *ast.NodeList
	if p.token == ast.KindOpenParenToken {
		argsList = p.parseArgumentList()
	}
	if p.opts.SkipZigDesugar {
		// Formatting parse: keep the builtin call source-faithful so the formatter does not add
		// spaces or move text around a synthesized expression.
		callee := p.newIdentifierAt("@"+name, core.NewTextRange(pos, pos+1+len(name)))
		return p.finishNode(p.factory.NewCallExpression(callee, nil, nil, argsList, ast.NodeFlagsNone), pos)
	}
	var args []*ast.Node
	if argsList != nil {
		args = argsList.Nodes
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
		if len(args) > 0 {
			if p.zigImportSpecLoc == nil {
				p.zigImportSpecLoc = make(map[*ast.Node]core.TextRange)
			}
			p.zigImportSpecLoc[expression] = args[0].Loc
		}
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
	case "typeInfo":
		// Lower to the global `typeInfo` from `builtin.ts`, which models `std.builtin.Type`.
		if len(args) == 1 {
			callee := p.newIdentifierAt("typeInfo", core.NewTextRange(pos, pos+len("@typeInfo")))
			argList := p.newNodeList(core.NewTextRange(pos, p.nodePos()), args)
			return p.finishNode(p.factory.NewCallExpression(callee, nil, nil, argList, ast.NodeFlagsNone), pos)
		}
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
	specLoc := core.NewTextRange(-1, -1)
	if loc, ok := p.zigImportSpecLoc[decl.Initializer]; ok {
		specLoc = loc
	}
	p.zigHoistedImports = append(p.zigHoistedImports, p.zigImportDeclaration(nameNode.Text(), spec, stmt.Pos(), specLoc))
	return true
}

// zigHoistAliasBinding recognizes a top-level `const Alias = OtherImport` and re-exports the
// imported module namespace under `Alias`, so `Alias.Member` resolves (a plain const alias would
// carry only the value meaning). Returns true when the statement was handled.
func (p *Parser) zigHoistAliasBinding(stmt *ast.Node) bool {
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
	if decl == nil || decl.Initializer == nil || decl.Initializer.Kind != ast.KindIdentifier {
		return false
	}
	spec, ok := p.zigImportSpecs[decl.Initializer.Text()]
	if !ok {
		return false
	}
	nameNode := decl.Name()
	if nameNode == nil || nameNode.Kind != ast.KindIdentifier {
		return false
	}
	exported := ast.HasSyntacticModifier(stmt, ast.ModifierFlagsExport)
	p.zigHoistedImports = append(p.zigHoistedImports, p.zigImportBindingDeclarations(nameNode, spec, exported, stmt.Pos(), core.NewTextRange(-1, -1))...)
	return true
}

// zigImportDeclaration synthesizes either `import * as Name from "./spec.zig"` or, for the
// non-module `builtin`/`root` specifiers, a permissive module-scope `const`.
func (p *Parser) zigImportDeclaration(name, spec string, pos int, specLoc core.TextRange) *ast.Node {
	if spec != "" && spec != "builtin" && spec != "root" {
		if p.zigImportSpecs == nil {
			p.zigImportSpecs = map[string]string{}
		}
		p.zigImportSpecs[name] = spec
	}
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
	hasSpecLoc := specLoc.Pos() >= 0 && specLoc.End() >= specLoc.Pos()
	if hasSpecLoc {
		// Keep the original specifier's source range so go-to-definition / Cmd+Click works.
		specLiteral.Loc = specLoc
	}
	// Bind the module namespace (`import * as X from "./y.zig"`), so `X.Member` resolves and the
	// binding keeps a real type. Unlike a default import, this does not require the target file to
	// default-export.
	namespaceImport := p.finishNodeWithEnd(p.factory.NewNamespaceImport(nameIdentifier), pos, pos)
	importClause := p.finishNodeWithEnd(p.factory.NewImportClause(ast.KindUnknown, nil, namespaceImport), pos, pos)
	end := pos
	if hasSpecLoc {
		end = specLoc.End()
	}
	importDecl := p.finishNodeWithEnd(p.factory.NewImportDeclaration(nil, importClause, specLiteral, nil), pos, end)
	// The binder may not see the synthesized clause; parent the specifier explicitly so the language
	// service can map a position on it back to the import declaration.
	specLiteral.Parent = importDecl
	return importDecl
}
