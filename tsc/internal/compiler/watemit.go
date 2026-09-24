package compiler

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/diagnostics"
	"github.com/microsoft/TypeScript/tsc/internal/wasmtext"
)

// emitWatJSFile emits the JavaScript for a WebAssembly text (`.wat`) source file. The `.wat` source is compiled to
// a WebAssembly binary, inlined as base64 and instantiated at module load; every WebAssembly export is then
// re-exported as a JavaScript binding.
func (e *emitter) emitWatJSFile(sourceFile *ast.SourceFile, jsFilePath string) {
	text, err := generateWatJS(sourceFile.Text())
	if err != nil {
		e.emitterDiagnostics.Add(ast.NewCompilerDiagnostic(diagnostics.Could_not_write_file_0_Colon_1, jsFilePath, err.Error()))
		return
	}
	if err := e.writeText(jsFilePath, text, &WriteFileData{SourceFile: sourceFile}); err != nil {
		e.emitterDiagnostics.Add(ast.NewCompilerDiagnostic(diagnostics.Could_not_write_file_0_Colon_1, jsFilePath, err.Error()))
		return
	}
	e.emitResult.EmittedFiles = append(e.emitResult.EmittedFiles, jsFilePath)
}

// emitWatDeclarationFile emits the declaration file for a `.wat` source file, describing the WebAssembly module's
// exports.
func (e *emitter) emitWatDeclarationFile(sourceFile *ast.SourceFile, declarationFilePath string) {
	text := generateWatDeclaration(sourceFile.Text())
	if err := e.writeText(declarationFilePath, text, &WriteFileData{SourceFile: sourceFile}); err != nil {
		e.emitterDiagnostics.Add(ast.NewCompilerDiagnostic(diagnostics.Could_not_write_file_0_Colon_1, declarationFilePath, err.Error()))
		return
	}
	e.emitResult.EmittedFiles = append(e.emitResult.EmittedFiles, declarationFilePath)
}

func generateWatJS(wat string) (string, error) {
	module, err := wasmtext.Parse(wat)
	if err != nil {
		return "", err
	}
	wasm, err := compileWatToWasm(wat)
	if err != nil {
		return "", err
	}

	var builder strings.Builder
	builder.WriteString("const __wasmBytes = Uint8Array.from(atob(\"")
	builder.WriteString(base64.StdEncoding.EncodeToString(wasm))
	builder.WriteString("\"), (c) => c.charCodeAt(0));\n")
	builder.WriteString("const __wasmModule = new WebAssembly.Module(__wasmBytes);\n")
	builder.WriteString("const __wasmInstance = new WebAssembly.Instance(__wasmModule);\n")
	for i, export := range module.Exports {
		localName := fmt.Sprintf("__wasmExport%d", i)
		builder.WriteString(fmt.Sprintf("const %s = __wasmInstance.exports[%s];\n", localName, strconv.Quote(export.Name)))
		builder.WriteString(fmt.Sprintf("export { %s as %s };\n", localName, export.TSName))
	}
	if len(module.Exports) == 0 {
		builder.WriteString("export {};\n")
	}
	return builder.String(), nil
}

func generateWatDeclaration(wat string) string {
	module, err := wasmtext.Parse(wat)
	if err != nil {
		return ""
	}
	var builder strings.Builder
	for _, export := range module.Exports {
		switch export.Kind {
		case wasmtext.ExportKindFunc:
			builder.WriteString("export declare function ")
			builder.WriteString(export.TSName)
			builder.WriteString("(")
			if export.Sig != nil {
				for i, paramType := range export.Sig.Params {
					if i > 0 {
						builder.WriteString(", ")
					}
					name := fmt.Sprintf("arg%d", i)
					if i < len(export.TSParamNames) {
						name = export.TSParamNames[i]
					}
					builder.WriteString(name)
					builder.WriteString(": ")
					builder.WriteString(wasmtext.ValueTypeString(paramType))
				}
			}
			builder.WriteString(")")
			builder.WriteString(watFunctionResultString(export.Sig))
			builder.WriteString(";\n")
		default:
			typeName := "Memory"
			switch export.Kind {
			case wasmtext.ExportKindGlobal:
				typeName = "Global"
			case wasmtext.ExportKindTable:
				typeName = "Table"
			}
			builder.WriteString("export declare const ")
			builder.WriteString(export.TSName)
			builder.WriteString(": WebAssembly.")
			builder.WriteString(typeName)
			builder.WriteString(";\n")
		}
	}
	return builder.String()
}

func watFunctionResultString(sig *wasmtext.FuncSignature) string {
	if sig == nil || len(sig.Results) == 0 {
		return ""
	}
	if len(sig.Results) == 1 {
		return ": " + wasmtext.ValueTypeString(sig.Results[0])
	}
	var builder strings.Builder
	builder.WriteString(": [")
	for i, result := range sig.Results {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(wasmtext.ValueTypeString(result))
	}
	builder.WriteString("]")
	return builder.String()
}

// compileWatToWasm compiles WebAssembly text to a WebAssembly binary by invoking an external tool. Either
// `wat2wasm` (from wabt) or `wasm-tools` must be available on the PATH.
func compileWatToWasm(wat string) ([]byte, error) {
	if _, err := exec.LookPath("wat2wasm"); err == nil {
		return runWatTool(wat, "wat2wasm", "-", "-o", "-")
	}
	if _, err := exec.LookPath("wasm-tools"); err == nil {
		return runWatTool(wat, "wasm-tools", "parse", "-")
	}
	return nil, errors.New("compiling .wat files requires either 'wat2wasm' or 'wasm-tools' to be installed and on the PATH")
}

func runWatTool(input string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(input)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, fmt.Errorf("%s failed: %w", name, err)
		}
		return nil, fmt.Errorf("%s failed: %w: %s", name, err, message)
	}
	return stdout.Bytes(), nil
}
