package zig_parser

import (
	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// nextTokenIsColon is a lookAhead callback used to recognize container field declarations.
func (p *Parser) nextTokenIsColon() bool {
	return p.nextToken() == ast.KindColonToken
}

// parseZigContainerMember parses a single member of a Zig container body. Fields
// (`name: Type = value;`) are parsed as property declarations so that they carry proper type
// information; every other member is an ordinary statement (function/const/var/enum declaration)
// that stays in the container's module body.
func (p *Parser) parseZigContainerMember() *ast.Node {
	if p.token == ast.KindIdentifier && p.lookAhead((*Parser).nextTokenIsColon) {
		pos := p.nodePos()
		jsdoc := p.jsdocScannerInfo()
		name := p.parsePropertyName()
		prop := p.parsePropertyDeclaration(pos, jsdoc, nil, name, nil)
		// Zig fields are always assigned when a value is constructed, so a field without a default
		// value is definitely assigned. Mark it with `!` so `strictPropertyInitialization` is happy.
		if data := prop.AsPropertyDeclaration(); data.Initializer == nil && data.PostfixToken == nil {
			token := p.factory.NewToken(ast.KindExclamationToken)
			token.Loc = core.NewTextRange(prop.End(), prop.End())
			token.Parent = prop
			data.PostfixToken = token
		}
		return prop
	}
	return p.parseStatement()
}

// desugarZigStructs rewrites `const X = struct { ... }` (and nested equivalents) into the
// class/interface/module declaration triple that models a Zig container. Doing this on real AST
// nodes (rather than rewriting text) keeps source positions intact for the language service.
func (p *Parser) desugarZigStructs(statements []*ast.Node) []*ast.Node {
	changed := false
	out := make([]*ast.Node, 0, len(statements))
	for _, stmt := range statements {
		if expanded, ok := p.expandZigStruct(stmt); ok {
			out = append(out, expanded...)
			changed = true
		} else {
			out = append(out, stmt)
		}
	}
	if !changed {
		return statements
	}
	return out
}

// expandZigStruct expands a single `const X = struct { ... }` variable statement. It returns false
// when the statement is not a Zig container declaration.
func (p *Parser) expandZigStruct(stmt *ast.Node) ([]*ast.Node, bool) {
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
	init := decl.Initializer
	if init == nil || init.Kind != ast.KindModuleExpression || !p.zigStructExprs[init] {
		return nil, false
	}
	nameNode := decl.Name()
	if nameNode == nil || nameNode.Kind != ast.KindIdentifier {
		return nil, false
	}
	body := init.AsModuleExpression().Body
	if body == nil || body.Kind != ast.KindModuleBlock {
		return nil, false
	}

	start, end := stmt.Pos(), stmt.End()
	exported := ast.HasSyntacticModifier(stmt, ast.ModifierFlagsExport)

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

	return []*ast.Node{classDecl, interfaceDecl, moduleDecl}, true
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
	id := p.factory.NewIdentifier(n.Text())
	id.Loc = n.Loc
	return id
}
