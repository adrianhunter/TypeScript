package zig_parser

import (
	"fmt"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/wasmtext"
)

// parseWatSourceFile parses a WebAssembly text (`.wat`) file into a real TypeScript source file whose exported
// declarations describe the exports of the WebAssembly module. This lets the language service provide normal
// TypeScript features (symbols, hovers, completions) for `.wat` files that have been associated with TypeScript.
func (p *Parser) parseWatSourceFile() *ast.SourceFile {
	module, err := wasmtext.Parse(p.sourceText)
	if err != nil {
		module = &wasmtext.Module{}
	}

	var statements []*ast.Node
	for _, export := range module.Exports {
		if export.TSName == "" {
			continue
		}
		if export.Kind == wasmtext.ExportKindFunc {
			statements = append(statements, p.newWatFunctionDeclaration(export, export.TSName))
		} else {
			statements = append(statements, p.newWatValueDeclaration(export, export.TSName))
		}
	}

	eof := p.factory.NewToken(ast.KindEndOfFile)
	eof.Loc = core.NewTextRange(len(p.sourceText), len(p.sourceText))
	statementList := p.factory.NewNodeList(statements)
	statementList.Loc = core.NewTextRange(0, len(p.sourceText))
	node := p.factory.NewSourceFile(p.opts, p.sourceText, statementList, eof)
	result := node.AsSourceFile()
	ast.SetParentInChildren(node)
	p.finishSourceFile(result, false)
	return result
}

// newWatFunctionDeclaration builds `export declare function name(a: number, b: bigint): number;` for an exported
// WebAssembly function.
func (p *Parser) newWatFunctionDeclaration(export wasmtext.Export, name string) *ast.Node {
	f := &p.factory
	nameNode := f.NewIdentifier(name)
	nameNode.Loc = core.NewTextRange(export.NamePos, export.NameEnd)

	var params []*ast.Node
	if export.Sig != nil {
		params = make([]*ast.Node, 0, len(export.Sig.Params))
		for i, paramType := range export.Sig.Params {
			paramName := fmt.Sprintf("arg%d", i)
			if i < len(export.TSParamNames) {
				paramName = export.TSParamNames[i]
			}
			paramNameNode := f.NewIdentifier(paramName)
			paramTypeNode := watValueTypeNode(f, paramType)
			params = append(params, f.NewParameterDeclaration(nil, nil, paramNameNode, nil, paramTypeNode, nil))
		}
	}

	modifiers := f.NewModifierList([]*ast.Node{
		f.NewModifier(ast.KindExportKeyword),
		f.NewModifier(ast.KindDeclareKeyword),
	})
	declaration := f.NewFunctionDeclaration(
		modifiers,
		nil, /*asteriskToken*/
		nameNode,
		nil, /*typeParameters*/
		f.NewNodeList(params),
		watResultTypeNode(f, export.Sig),
		nil, /*fullSignature*/
		nil, /*body*/
	)
	declaration.Loc = core.NewTextRange(export.NamePos, export.NameEnd)
	declaration.Flags |= ast.NodeFlagsAmbient
	return declaration
}

// newWatValueDeclaration builds `export declare const name: WebAssembly.Memory;` for an exported memory, global or
// table.
func (p *Parser) newWatValueDeclaration(export wasmtext.Export, name string) *ast.Node {
	f := &p.factory
	nameNode := f.NewIdentifier(name)
	nameNode.Loc = core.NewTextRange(export.NamePos, export.NameEnd)

	typeName := "Memory"
	switch export.Kind {
	case wasmtext.ExportKindGlobal:
		typeName = "Global"
	case wasmtext.ExportKindTable:
		typeName = "Table"
	}
	typeNode := f.NewTypeReferenceNode(
		f.NewQualifiedName(f.NewIdentifier("WebAssembly"), f.NewIdentifier(typeName)),
		nil, /*typeArguments*/
	)

	declaration := f.NewVariableDeclaration(nameNode, nil, typeNode, nil)
	declaration.Loc = core.NewTextRange(export.NamePos, export.NameEnd)
	declarationList := f.NewVariableDeclarationList(f.NewNodeList([]*ast.Node{declaration}), ast.NodeFlagsConst)
	modifiers := f.NewModifierList([]*ast.Node{
		f.NewModifier(ast.KindExportKeyword),
		f.NewModifier(ast.KindDeclareKeyword),
	})
	statement := f.NewVariableStatement(modifiers, declarationList)
	statement.Loc = core.NewTextRange(export.NamePos, export.NameEnd)
	statement.Flags |= ast.NodeFlagsAmbient
	return statement
}

func watValueTypeNode(f *ast.NodeFactory, t wasmtext.ValueType) *ast.Node {
	switch t {
	case wasmtext.ValueTypeI64:
		return f.NewKeywordTypeNode(ast.KindBigIntKeyword)
	case wasmtext.ValueTypeI32, wasmtext.ValueTypeF32, wasmtext.ValueTypeF64:
		return f.NewKeywordTypeNode(ast.KindNumberKeyword)
	default:
		return f.NewKeywordTypeNode(ast.KindAnyKeyword)
	}
}

func watResultTypeNode(f *ast.NodeFactory, sig *wasmtext.FuncSignature) *ast.Node {
	if sig == nil || len(sig.Results) == 0 {
		return nil
	}
	if len(sig.Results) == 1 {
		return watValueTypeNode(f, sig.Results[0])
	}
	elements := make([]*ast.Node, 0, len(sig.Results))
	for _, result := range sig.Results {
		elements = append(elements, watValueTypeNode(f, result))
	}
	return f.NewTupleTypeNode(f.NewNodeList(elements))
}
