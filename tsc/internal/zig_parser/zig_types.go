package zig_parser

import (
	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// This file lowers Zig type syntax that has no direct TypeScript equivalent. It is used by the
// strict TypeScript-dialect parser so that genuine Zig files (such as the ones that exercise the
// language service) can still produce meaningful declaration output instead of `any`.

// zigNumericTypeNames are the Zig builtin types that lower to TypeScript's `number`.
var zigNumericTypeNames = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true, "i128": true, "isize": true,
	"u8": true, "u16": true, "u32": true, "u64": true, "u128": true, "usize": true,
	"f16": true, "f32": true, "f64": true, "f80": true, "f128": true,
	"comptime_int": true, "comptime_float": true,
	"c_char": true, "c_short": true, "c_ushort": true, "c_int": true, "c_uint": true,
	"c_long": true, "c_ulong": true, "c_longlong": true, "c_ulonglong": true,
	"c_longdouble": true,
}

// zigBuiltinTypeKind maps a Zig builtin type name onto the TypeScript keyword type that models it.
func zigBuiltinTypeKind(name string) (ast.Kind, bool) {
	switch name {
	case "bool":
		return ast.KindBooleanKeyword, true
	case "void":
		return ast.KindVoidKeyword, true
	case "noreturn":
		return ast.KindNeverKeyword, true
	case "anytype":
		return ast.KindUnknownKeyword, true
	case "anyframe", "type", "anyopaque":
		return ast.KindUnknownKeyword, true
	}
	if zigNumericTypeNames[name] {
		return ast.KindNumberKeyword, true
	}
	return ast.KindUnknown, false
}

// zigBuiltinTypeAtCurrent reports the keyword type for the current builtin type name. Qualified names
// such as `std.mem.Allocator` are left alone so they stay type references.
func (p *Parser) zigBuiltinTypeAtCurrent() (ast.Kind, bool) {
	if p.token != ast.KindIdentifier {
		return ast.KindUnknown, false
	}
	kind, ok := zigBuiltinTypeKind(p.scanner.TokenValue())
	if !ok {
		return ast.KindUnknown, false
	}
	if p.lookAhead((*Parser).nextTokenIsDot) {
		return ast.KindUnknown, false
	}
	return kind, true
}

// zigIsUndefinedExpression reports whether the expression is Zig's `undefined` initializer.
func (p *Parser) zigIsUndefinedExpression(expression *ast.Node) bool {
	return expression != nil && expression.Kind == ast.KindIdentifier && expression.Text() == "undefined"
}

// parseZigOptionalType parses a Zig optional type `?T` and lowers it to `T | null`.
func (p *Parser) parseZigOptionalType() *ast.Node {
	pos := p.nodePos()
	p.nextToken() // '?'
	inner := p.parseTypeOperatorOrHigher()
	nullExpr := p.factory.NewToken(ast.KindNullKeyword)
	nullExpr.Loc = core.NewTextRange(-1, -1)
	nullType := p.finishNodeWithEnd(p.factory.NewLiteralTypeNode(nullExpr), -1, -1)
	types := p.newNodeList(core.NewTextRange(pos, p.nodePos()), []*ast.Node{inner, nullType})
	return p.finishNodeWithEnd(p.factory.NewUnionTypeNode(types), pos, p.nodePos())
}

// zigStringTypeAt synthesizes a `string` keyword type anchored at pos.
func (p *Parser) zigStringTypeAt(pos int) *ast.Node {
	return p.finishNode(p.factory.NewKeywordTypeNode(ast.KindStringKeyword), pos)
}

// zigSkipTypeQualifiers consumes leading `const`/`volatile`/`allowzero` qualifiers.
func (p *Parser) zigSkipTypeQualifiers() {
	for {
		switch {
		case p.token == ast.KindConstKeyword:
			p.nextToken()
		case p.zigIsIdent("volatile"), p.zigIsIdent("allowzero"):
			p.nextToken()
		default:
			return
		}
	}
}

// zigSkipTypeModifiers consumes `align(...)`, `addrspace(...)` and similar postfix modifiers.
func (p *Parser) zigSkipTypeModifiers() {
	for p.zigIsIdent("align") || p.zigIsIdent("addrspace") || p.zigIsIdent("linksection") {
		p.nextToken()
		p.zigSkipBalanced(ast.KindOpenParenToken, ast.KindCloseParenToken)
	}
}

// parseZigArrayOrSliceType parses Zig prefix array and slice types: `[]T`, `[]const T`, `[N]T`,
// `[N:sentinel]T`, `[:sentinel]T` and `[*]T`. Slices of `u8` lower to `string`; other arrays and
// slices lower to `T[]`.
func (p *Parser) parseZigArrayOrSliceType() *ast.Node {
	pos := p.nodePos()
	p.parseExpected(ast.KindOpenBracketToken)
	if p.token == ast.KindAsteriskToken {
		// `[*]T` is a many-item pointer.
		p.nextToken()
	}
	isSlice := false
	switch {
	case p.token == ast.KindCloseBracketToken:
		isSlice = true
	case p.token == ast.KindColonToken:
		// `[:sentinel]T`
		p.nextToken()
		if p.token != ast.KindCloseBracketToken {
			p.zigSkipTypeUntil(ast.KindCloseBracketToken)
		}
		isSlice = true
	default:
		// `[N]T` or `[N:sentinel]T`
		p.zigSkipTypeUntil(ast.KindCloseBracketToken, ast.KindColonToken)
		if p.token == ast.KindColonToken {
			p.nextToken()
			if p.token != ast.KindCloseBracketToken {
				p.zigSkipTypeUntil(ast.KindCloseBracketToken)
			}
		}
	}
	p.parseExpected(ast.KindCloseBracketToken)
	p.zigSkipTypeQualifiers()
	p.zigSkipTypeModifiers()
	if isSlice && p.token == ast.KindIdentifier && p.scanner.TokenValue() == "u8" {
		p.nextToken()
		return p.zigStringTypeAt(pos)
	}
	elementType := p.parseType()
	return p.finishNode(p.factory.NewArrayTypeNode(elementType), pos)
}

// parseZigPointerType parses `*T`, `*const T` and friends. The pointee type is preserved, matching
// the reference Zig lowering.
func (p *Parser) parseZigPointerType() *ast.Node {
	p.parseExpected(ast.KindAsteriskToken)
	p.zigSkipTypeQualifiers()
	p.zigSkipTypeModifiers()
	return p.parseTypeOperatorOrHigher()
}

// zigExpressionToTypeNode converts a value expression written in type position (`Vec2`,
// `std.mem.Allocator`) into the equivalent type node, or nil when it is not a simple name.
func (p *Parser) zigExpressionToTypeNode(expression *ast.Node) *ast.Node {
	entity := p.zigExpressionToEntityName(expression)
	if entity == nil {
		return nil
	}
	return p.finishNode(p.factory.NewTypeReferenceNode(entity, nil), expression.Pos())
}

// zigExpressionToEntityName converts an identifier or property-access chain into an entity name.
func (p *Parser) zigExpressionToEntityName(expression *ast.Node) *ast.Node {
	switch expression.Kind {
	case ast.KindIdentifier:
		return p.newIdentifierLike(expression)
	case ast.KindPropertyAccessExpression:
		propertyAccess := expression.AsPropertyAccessExpression()
		left := p.zigExpressionToEntityName(propertyAccess.Expression)
		if left == nil {
			return nil
		}
		right := propertyAccess.Name()
		if right == nil || right.Kind != ast.KindIdentifier {
			return nil
		}
		return p.finishNodeWithEnd(p.factory.NewQualifiedName(left, p.newIdentifierLike(right)), left.Pos(), right.End())
	}
	return nil
}

// parseZigEnumExpression parses an `enum { ... }` container expression. The members are recorded so
// the surrounding `const X = enum { ... }` declaration can be desugared into a union type plus a
// value namespace.
func (p *Parser) parseZigEnumExpression() *ast.Expression {
	pos := p.nodePos()
	openBracePosition := p.scanner.TokenStart()
	p.parseExpected(ast.KindEnumKeyword)
	openBraceParsed := p.parseExpected(ast.KindOpenBraceToken)
	var members []string
	for p.token != ast.KindCloseBraceToken && p.token != ast.KindEndOfFile {
		if p.parseOptional(ast.KindCommaToken) || p.parseOptional(ast.KindSemicolonToken) {
			continue
		}
		if p.token != ast.KindIdentifier {
			p.nextToken()
			continue
		}
		members = append(members, p.scanner.TokenValue())
		p.nextToken()
		if p.parseOptional(ast.KindEqualsToken) {
			p.zigSkipTypeUntil(ast.KindCommaToken, ast.KindCloseBraceToken)
		}
	}
	p.parseExpectedMatchingBrackets(ast.KindOpenBraceToken, ast.KindCloseBraceToken, openBraceParsed, openBracePosition)
	body := p.finishNode(p.factory.NewModuleBlock(p.newNodeList(core.NewTextRange(p.nodePos(), p.nodePos()), nil)), p.nodePos())
	result := p.finishNode(p.factory.NewModuleExpression(body), pos)
	if p.zigEnumExprs == nil {
		p.zigEnumExprs = make(map[*ast.Node][]string)
	}
	p.zigEnumExprs[result] = members
	return result
}
