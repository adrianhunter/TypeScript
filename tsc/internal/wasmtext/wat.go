// Package wasmtext implements a small scanner and parser for the WebAssembly text format (`.wat`).
//
// Only the parts of the module that are needed to describe a WebAssembly module's public interface are
// interpreted: the exports, together with the signatures of exported functions and the kinds of exported
// memories, globals and tables. The body of each function is scanned but not compiled here.
package wasmtext

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValueType identifies a WebAssembly value type.
type ValueType int

const (
	ValueTypeUnknown ValueType = iota
	ValueTypeI32
	ValueTypeI64
	ValueTypeF32
	ValueTypeF64
	ValueTypeV128
	ValueTypeFuncRef
	ValueTypeExternRef
)

// ExportKind identifies the kind of a WebAssembly module export.
type ExportKind int

const (
	ExportKindFunc ExportKind = iota
	ExportKindMemory
	ExportKindGlobal
	ExportKindTable
)

// FuncSignature describes the parameters and results of a WebAssembly function.
type FuncSignature struct {
	Params     []ValueType
	ParamNames []string
	Results    []ValueType
}

// Export describes a single export of a WebAssembly module.
type Export struct {
	Name    string
	Kind    ExportKind
	Sig     *FuncSignature
	NamePos int
	NameEnd int
	// TSName is the (sanitized, unique) name under which this export is surfaced as a TypeScript binding.
	TSName string
	// TSParamNames holds the (sanitized, unique) parameter names for exported functions.
	TSParamNames []string
}

// Module is the subset of a parsed WebAssembly text module that is relevant to TypeScript.
type Module struct {
	Exports []Export
}

// token kinds produced by the scanner.
type tokenKind int

const (
	tokenEOF tokenKind = iota
	tokenLParen
	tokenRParen
	tokenAtom
	tokenString
)

type token struct {
	kind tokenKind
	text string
	pos  int
	end  int
}

type scanner struct {
	text string
	pos  int
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
}

func isAtomDelimiter(c byte) bool {
	return isSpace(c) || c == '(' || c == ')' || c == '"' || c == ';'
}

// scan returns all tokens in the text, or an error if a string literal is unterminated.
func scan(text string) ([]token, error) {
	s := &scanner{text: text}
	var tokens []token
	for {
		s.skipTrivia()
		if s.pos >= len(s.text) {
			tokens = append(tokens, token{kind: tokenEOF, pos: s.pos, end: s.pos})
			return tokens, nil
		}
		start := s.pos
		switch c := s.text[s.pos]; c {
		case '(':
			s.pos++
			tokens = append(tokens, token{kind: tokenLParen, text: "(", pos: start, end: s.pos})
		case ')':
			s.pos++
			tokens = append(tokens, token{kind: tokenRParen, text: ")", pos: start, end: s.pos})
		case '"':
			value, err := s.scanString()
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokenString, text: value, pos: start, end: s.pos})
		default:
			if isAtomDelimiter(s.text[s.pos]) {
				// A lone delimiter (such as a stray `;`) that is not part of a comment. Consume it so that scanning
				// always makes progress, even on malformed input.
				s.pos++
			} else {
				for s.pos < len(s.text) && !isAtomDelimiter(s.text[s.pos]) {
					s.pos++
				}
			}
			tokens = append(tokens, token{kind: tokenAtom, text: s.text[start:s.pos], pos: start, end: s.pos})
		}
	}
}

func (s *scanner) skipTrivia() {
	for s.pos < len(s.text) {
		c := s.text[s.pos]
		if isSpace(c) {
			s.pos++
			continue
		}
		if c == ';' && s.pos+1 < len(s.text) && s.text[s.pos+1] == ';' {
			s.pos += 2
			for s.pos < len(s.text) && s.text[s.pos] != '\n' {
				s.pos++
			}
			continue
		}
		if c == '(' && s.pos+1 < len(s.text) && s.text[s.pos+1] == ';' {
			s.pos += 2
			depth := 1
			for s.pos < len(s.text) && depth > 0 {
				if s.text[s.pos] == '(' && s.pos+1 < len(s.text) && s.text[s.pos+1] == ';' {
					depth++
					s.pos += 2
				} else if s.text[s.pos] == ';' && s.pos+1 < len(s.text) && s.text[s.pos+1] == ')' {
					depth--
					s.pos += 2
				} else {
					s.pos++
				}
			}
			continue
		}
		break
	}
}

func (s *scanner) scanString() (string, error) {
	// s.text[s.pos] == '"'
	s.pos++
	var sb strings.Builder
	for s.pos < len(s.text) {
		c := s.text[s.pos]
		if c == '"' {
			s.pos++
			return sb.String(), nil
		}
		if c == '\\' {
			s.pos++
			if s.pos >= len(s.text) {
				break
			}
			escaped := s.text[s.pos]
			s.pos++
			switch escaped {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '"':
				sb.WriteByte('"')
			case '\'':
				sb.WriteByte('\'')
			case '\\':
				sb.WriteByte('\\')
			case 'u':
				var value rune
				for i := 0; i < 4 && s.pos < len(s.text); i++ {
					digit := hexDigit(s.text[s.pos])
					if digit < 0 {
						break
					}
					value = value*16 + rune(digit)
					s.pos++
				}
				sb.WriteRune(value)
			default:
				if digit := hexDigit(escaped); digit >= 0 {
					value := digit
					if s.pos < len(s.text) {
						if digit2 := hexDigit(s.text[s.pos]); digit2 >= 0 {
							value = value*16 + digit2
							s.pos++
						}
					}
					sb.WriteByte(byte(value))
				} else {
					sb.WriteByte(escaped)
				}
			}
			continue
		}
		sb.WriteByte(c)
		s.pos++
	}
	return "", fmt.Errorf("unterminated string literal")
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// node is a generic s-expression node.
type node struct {
	atom     string
	str      string
	isString bool
	list     []*node
	isList   bool
	pos      int
	end      int
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() token {
	return p.tokens[p.pos]
}

func (p *parser) next() token {
	t := p.tokens[p.pos]
	if t.kind != tokenEOF {
		p.pos++
	}
	return t
}

func (p *parser) parseNode() *node {
	t := p.next()
	switch t.kind {
	case tokenLParen:
		n := &node{isList: true, pos: t.pos}
		for p.peek().kind != tokenRParen && p.peek().kind != tokenEOF {
			n.list = append(n.list, p.parseNode())
		}
		end := t.end
		if p.peek().kind == tokenRParen {
			close := p.next()
			end = close.end
		}
		n.end = end
		return n
	case tokenString:
		return &node{isString: true, str: t.text, pos: t.pos, end: t.end}
	default:
		return &node{atom: t.text, pos: t.pos, end: t.end}
	}
}

// keyword returns the leading atom of a list node, or "" when the node is not a list.
func (n *node) keyword() string {
	if n == nil || !n.isList || len(n.list) == 0 || n.list[0].isList || n.list[0].isString {
		return ""
	}
	return n.list[0].atom
}

func parseValueType(atom string) ValueType {
	switch atom {
	case "i32":
		return ValueTypeI32
	case "i64":
		return ValueTypeI64
	case "f32":
		return ValueTypeF32
	case "f64":
		return ValueTypeF64
	case "v128":
		return ValueTypeV128
	case "funcref":
		return ValueTypeFuncRef
	case "externref":
		return ValueTypeExternRef
	}
	return ValueTypeUnknown
}

// Parse scans and parses a WebAssembly text module, returning the module's public interface.
func Parse(text string) (*Module, error) {
	tokens, err := scan(text)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	var roots []*node
	for p.peek().kind != tokenEOF {
		roots = append(roots, p.parseNode())
	}
	fields := roots
	if len(roots) == 1 && roots[0].keyword() == "module" {
		fields = roots[0].list[1:]
	}
	return buildModule(fields), nil
}

type moduleBuilder struct {
	module       *Module
	funcs        []*FuncSignature
	funcNames    map[string]int
	memoryCount  int
	globalCount  int
	tableCount   int
	exportedName map[string]bool
}

func buildModule(fields []*node) *Module {
	b := &moduleBuilder{
		module:    &Module{},
		funcNames: map[string]int{},
	}
	for _, field := range fields {
		b.addField(field)
	}
	assignTypeScriptNames(b.module)
	return b.module
}

// assignTypeScriptNames computes a valid, unique TypeScript identifier for every export.
func assignTypeScriptNames(module *Module) {
	used := map[string]bool{}
	for i := range module.Exports {
		export := &module.Exports[i]
		name := uniqueIdentifier(sanitizeIdentifier(export.Name), used)
		if name == "" {
			name = uniqueIdentifier("wasmExport", used)
		}
		export.TSName = name
		if export.Sig == nil {
			continue
		}
		paramUsed := map[string]bool{}
		for p := range export.Sig.Params {
			paramName := ""
			if p < len(export.Sig.ParamNames) {
				paramName = sanitizeIdentifier(export.Sig.ParamNames[p])
			}
			if paramName == "" {
				paramName = fmt.Sprintf("arg%d", p)
			}
			export.TSParamNames = append(export.TSParamNames, uniqueIdentifier(paramName, paramUsed))
		}
	}
}

// ValueTypeString returns the TypeScript type used to represent a WebAssembly value type.
func ValueTypeString(t ValueType) string {
	switch t {
	case ValueTypeI64:
		return "bigint"
	case ValueTypeI32, ValueTypeF32, ValueTypeF64:
		return "number"
	default:
		return "any"
	}
}

func (b *moduleBuilder) addField(field *node) {
	switch field.keyword() {
	case "import":
		b.addImport(field)
	case "func":
		b.addFunc(field)
	case "memory":
		b.memoryCount++
		b.addInlineExports(field, ExportKindMemory, nil)
	case "global":
		b.globalCount++
		b.addInlineExports(field, ExportKindGlobal, nil)
	case "table":
		b.tableCount++
		b.addInlineExports(field, ExportKindTable, nil)
	case "export":
		b.addExport(field)
	}
}

func (b *moduleBuilder) addImport(field *node) {
	// (import "module" "name" (func $id ...)) | (import ... (memory ...)) | ...
	for _, desc := range field.list {
		if !desc.isList {
			continue
		}
		switch desc.keyword() {
		case "func":
			sig := parseSignature(desc)
			id := funcID(desc)
			index := len(b.funcs)
			b.funcs = append(b.funcs, sig)
			if id != "" {
				b.funcNames[id] = index
			}
		case "memory":
			b.memoryCount++
		case "global":
			b.globalCount++
		case "table":
			b.tableCount++
		}
	}
}

func (b *moduleBuilder) addFunc(field *node) {
	sig := parseSignature(field)
	index := len(b.funcs)
	b.funcs = append(b.funcs, sig)
	if id := funcID(field); id != "" {
		b.funcNames[id] = index
	}
	b.addInlineExports(field, ExportKindFunc, sig)
}

// addInlineExports records the exports declared inline on a definition, e.g. `(func (export "f") ...)`.
func (b *moduleBuilder) addInlineExports(field *node, kind ExportKind, sig *FuncSignature) {
	if !field.isList {
		return
	}
	for _, child := range field.list {
		if child.keyword() != "export" {
			continue
		}
		name, ok := exportName(child)
		if !ok {
			continue
		}
		b.recordExport(name, kind, child, sig)
	}
}

func (b *moduleBuilder) addExport(field *node) {
	// (export "name" (func $id)) | (export "name" (memory $id)) | ...
	name, ok := exportName(field)
	if !ok {
		return
	}
	kind := ExportKindFunc
	var sig *FuncSignature
	for _, desc := range field.list {
		if !desc.isList {
			continue
		}
		switch desc.keyword() {
		case "func":
			kind = ExportKindFunc
			sig = b.lookupFunc(desc)
		case "memory":
			kind = ExportKindMemory
		case "global":
			kind = ExportKindGlobal
		case "table":
			kind = ExportKindTable
		}
	}
	b.recordExport(name, kind, field, sig)
}

func (b *moduleBuilder) recordExport(name string, kind ExportKind, field *node, sig *FuncSignature) {
	if b.exportedName == nil {
		b.exportedName = map[string]bool{}
	}
	if b.exportedName[name] {
		return
	}
	b.exportedName[name] = true
	namePos, nameEnd := exportNameRange(field)
	b.module.Exports = append(b.module.Exports, Export{
		Name:    name,
		Kind:    kind,
		Sig:     sig,
		NamePos: namePos,
		NameEnd: nameEnd,
	})
}

func (b *moduleBuilder) lookupFunc(desc *node) *FuncSignature {
	// desc is either a `(func $id)` reference or a definition; find the index reference.
	for _, child := range desc.list[1:] {
		if child.isList {
			continue
		}
		if index, ok := b.funcIndex(child.atom); ok {
			return b.funcs[index]
		}
	}
	return nil
}

func (b *moduleBuilder) funcIndex(ref string) (int, bool) {
	if len(ref) > 0 && ref[0] == '$' {
		index, ok := b.funcNames[ref]
		return index, ok
	}
	var index int
	if _, err := fmt.Sscanf(ref, "%d", &index); err == nil {
		if index >= 0 && index < len(b.funcs) {
			return index, true
		}
	}
	return 0, false
}

func funcID(field *node) string {
	if len(field.list) < 2 {
		return ""
	}
	id := field.list[1]
	if !id.isList && !id.isString && len(id.atom) > 0 && id.atom[0] == '$' {
		return id.atom
	}
	return ""
}

// parseSignature extracts the parameters and results declared inline on a function-ish node.
func parseSignature(field *node) *FuncSignature {
	sig := &FuncSignature{}
	for _, child := range field.list {
		if !child.isList {
			continue
		}
		switch child.keyword() {
		case "param":
			parseParamGroup(child, sig)
		case "result":
			for _, t := range child.list[1:] {
				if !t.isList && !t.isString {
					sig.Results = append(sig.Results, parseValueType(t.atom))
				}
			}
		}
	}
	return sig
}

func parseParamGroup(group *node, sig *FuncSignature) {
	args := group.list[1:]
	named := false
	for _, arg := range args {
		if arg.isList || arg.isString {
			continue
		}
		if len(arg.atom) > 0 && arg.atom[0] == '$' {
			named = true
			break
		}
	}
	if named {
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if arg.isList || arg.isString || len(arg.atom) == 0 || arg.atom[0] != '$' {
				continue
			}
			if i+1 < len(args) {
				typeAtom := args[i+1]
				sig.Params = append(sig.Params, parseValueType(typeAtom.atom))
				sig.ParamNames = append(sig.ParamNames, strings.TrimPrefix(arg.atom, "$"))
				i++
			}
		}
		return
	}
	for _, arg := range args {
		if arg.isList || arg.isString {
			continue
		}
		sig.Params = append(sig.Params, parseValueType(arg.atom))
		sig.ParamNames = append(sig.ParamNames, "")
	}
}

// exportName returns the string literal name of an `(export ...)` node.
func exportName(field *node) (string, bool) {
	if !field.isList {
		return "", false
	}
	for _, child := range field.list[1:] {
		if child.isString {
			return child.str, true
		}
	}
	return "", false
}

// exportNameRange returns the position of the exported string literal.
func exportNameRange(field *node) (int, int) {
	if !field.isList {
		return 0, 0
	}
	for _, child := range field.list[1:] {
		if child.isString {
			return child.pos, child.end
		}
	}
	return field.pos, field.end
}

// sanitizeIdentifier converts an arbitrary WebAssembly export name into something that can be used as a
// TypeScript identifier.
func sanitizeIdentifier(name string) string {
	if name == "" {
		return ""
	}
	var builder strings.Builder
	for i, r := range name {
		valid := r == '_' || r == '$' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r))
		if valid {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	result := builder.String()
	if result == "" {
		return ""
	}
	if first, _ := utf8.DecodeRuneInString(result); !unicode.IsLetter(first) && first != '_' && first != '$' {
		result = "_" + result
	}
	return result
}

func uniqueIdentifier(name string, used map[string]bool) string {
	if name == "" {
		return ""
	}
	candidate := name
	for i := 1; used[candidate]; i++ {
		candidate = fmt.Sprintf("%s_%d", name, i)
	}
	used[candidate] = true
	return candidate
}
