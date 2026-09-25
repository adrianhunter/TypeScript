package main

import (
	"fmt"
	"os"

	"github.com/microsoft/TypeScript/tsc/internal/zig_parser"
)

func main() {
	data, _ := os.ReadFile(os.Args[1])
	fmt.Print(zig_parser.TranslateZig(string(data)))
}
