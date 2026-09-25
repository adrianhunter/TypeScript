package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/binder"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/printer"
	"github.com/microsoft/TypeScript/tsc/internal/sourcemap"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
)

// runZigDebug prints the lowered TypeScript for a Zig file together with a source map linking the
// generated text back to the original Zig source. Used by the editor's "Show Generated TypeScript"
// debug command (the equivalent of Volar's virtual-file view).
//
// Usage: tsc --zig-debug <file.zig>
func runZigDebug(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tsc --zig-debug <file.zig>")
		return 1
	}
	path := args[0]
	text, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	currentDirectory, _ := os.Getwd()
	path = tspath.GetNormalizedAbsolutePath(path, currentDirectory)

	sourceFile := parser.ParseSourceFile(ast.SourceFileParseOptions{FileName: path}, string(text), core.ScriptKindZig)
	// The printer walks parent pointers for context; the binder establishes them.
	binder.BindSourceFile(sourceFile)

	writer := printer.NewTextWriter("\n", 4)
	generator := sourcemap.NewGenerator(
		tspath.GetBaseFileName(tspath.NormalizeSlashes(path)),
		"",
		"",
		tspath.ComparePathsOptions{UseCaseSensitiveFileNames: true},
	)
	p := printer.NewPrinter(
		printer.PrinterOptions{NewLine: core.NewLineKindLF},
		printer.PrintHandlers{},
		printer.NewEmitContext(),
	)
	p.Write(sourceFile.AsNode(), sourceFile, writer, generator)

	output := struct {
		Code string `json:"code"`
		Map  any    `json:"map"`
	}{Code: writer.String(), Map: generator.RawSourceMap()}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
