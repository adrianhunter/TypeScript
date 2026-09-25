package zig_parser

import (
	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// This file lowers Zig labeled-block expressions (`label: { ... break :label value; }`). They are
// emitted as an immediately-invoked arrow function so the block can produce a value and `break
// :label` can be lowered to `return`.

// nextTokenIsColonThenOpenBrace reports whether the upcoming tokens are `: {`, the start of a Zig
// labeled block.
func (p *Parser) nextTokenIsColonThenOpenBrace() bool {
	if p.nextToken() != ast.KindColonToken {
		return false
	}
	return p.nextToken() == ast.KindOpenBraceToken
}

// parseZigLabeledBlockExpression parses `label: { ... }` and lowers it to `(() => { ... })()`.
func (p *Parser) parseZigLabeledBlockExpression() *ast.Expression {
	pos := p.nodePos()
	label := p.parseIdentifier()
	p.parseExpected(ast.KindColonToken)
	p.zigBlockLabels = append(p.zigBlockLabels, label.Text())
	body := p.parseBlock(false /*ignoreMissingOpenBrace*/, nil /*diagnosticMessage*/)
	p.zigBlockLabels = p.zigBlockLabels[:len(p.zigBlockLabels)-1]

	equalsGreaterThan := p.factory.NewToken(ast.KindEqualsGreaterThanToken)
	// The checker slices source text using this token's range, so it must have a real (non-negative)
	// position even though it is synthesized.
	equalsGreaterThan.Loc = core.NewTextRange(pos, pos)
	parameters := p.newNodeList(core.NewTextRange(-1, -1), nil)
	arrow := p.finishNodeWithEnd(p.factory.NewArrowFunction(
		nil, nil, parameters, nil, nil, equalsGreaterThan, body,
	), -1, -1)
	callee := p.finishNodeWithEnd(p.factory.NewParenthesizedExpression(arrow), -1, -1)
	arguments := p.newNodeList(core.NewTextRange(-1, -1), nil)
	return p.finishNodeWithEnd(p.factory.NewCallExpression(callee, nil, nil, arguments, ast.NodeFlagsNone), pos, p.nodePos())
}
