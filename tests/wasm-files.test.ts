import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
    readFileSync,
    writeFileSync,
} from "node:fs";
import path from "node:path";
import {
    before,
    describe,
    it,
} from "node:test";
import {
    buildTsc,
    compile,
    diagnostics,
} from "./helpers.ts";

function wasmToolAvailable(): boolean {
    return spawnSync("wat2wasm", ["--version"]).status === 0 || spawnSync("wasm-tools", ["--version"]).status === 0;
}

const hasWasmTool = wasmToolAvailable();
const skip = hasWasmTool ? false : "neither wat2wasm nor wasm-tools is installed";

before(async () => {
    await buildTsc();
});

const addWat = `(module
  (func $add (export "add") (param i32 i32) (result i32)
    local.get 0
    local.get 1
    i32.add)
)
`;

const richWat = `(module
  (memory (export "memory") 1)
  (func $add (export "add") (param $a i32) (param $b i32) (result i32)
    local.get $a
    local.get $b
    i32.add)
  (func $mul (param i64 i64) (result i64)
    local.get 0
    local.get 1
    i64.mul)
  (export "mul" (func $mul))
  (func $swap (param i32 i32) (result i32 i32)
    local.get 1
    local.get 0)
  (export "swap" (func $swap))
)
`;

describe("wasm (.wat) source files", () => {
    it("compiles a .wat module to an inlined WebAssembly instance and re-exports its exports", { skip }, () => {
        const result = compile({ "add.wat": addWat });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("add.js");
        assert.match(js, /new WebAssembly\.Module\(/);
        assert.match(js, /new WebAssembly\.Instance\(/);
        assert.match(js, /__wasmInstance\.exports\["add"\]/);
        assert.match(js, /export \{ __wasmExport0 as add \};/);

        const base64Match = js.match(/atob\("([A-Za-z0-9+/=]+)"\)/);
        assert.ok(base64Match, `expected a base64 Wasm payload in the emitted JS:\n${js}`);
        const wasm = Buffer.from(base64Match[1], "base64");
        assert.deepEqual([...wasm.subarray(0, 4)], [0x00, 0x61, 0x73, 0x6d], "emitted payload should be a WebAssembly binary");

        const dts = result.read("add.d.wat.ts");
        assert.match(dts, /export declare function add\(arg0: number, arg1: number\): number;/);
    });

    it("supports memories, named parameters, i64 and multi-value results", { skip }, () => {
        const result = compile({ "rich.wat": richWat });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const dts = result.read("rich.d.wat.ts");
        assert.match(dts, /export declare const memory: WebAssembly\.Memory;/);
        assert.match(dts, /export declare function add\(a: number, b: number\): number;/);
        assert.match(dts, /export declare function mul\(arg0: bigint, arg1: bigint\): bigint;/);
        assert.match(dts, /export declare function swap\(arg0: number, arg1: number\): \[number, number\];/);

        const js = result.read("rich.js");
        assert.match(js, /__wasmInstance\.exports\["memory"\]/);
        assert.match(js, /__wasmInstance\.exports\["mul"\]/);
        assert.match(js, /__wasmInstance\.exports\["swap"\]/);
    });

    it("sanitizes export names that are not valid identifiers", { skip }, () => {
        const result = compile({
            "weird.wat": `(module
  (func $f (result i32) (i32.const 7))
  (export "kebab-case" (func $f))
  (export "has space" (func $f))
)
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const dts = result.read("weird.d.wat.ts");
        assert.match(dts, /export declare function kebab_case\(\): number;/);
        assert.match(dts, /export declare function has_space\(\): number;/);

        const js = result.read("weird.js");
        assert.match(js, /__wasmInstance\.exports\["kebab-case"\]/);
        assert.match(js, /export \{ __wasmExport0 as kebab_case \};/);
        assert.match(js, /__wasmInstance\.exports\["has space"\]/);
        assert.match(js, /export \{ __wasmExport1 as has_space \};/);
    });

    it("lets TypeScript import and type-check a .wat module", { skip }, () => {
        const result = compile({
            "add.wat": addWat,
            "consumer.ts": `import { add } from "./add.wat";

export const result: number = add(1, 2);
`,
        }, { allowArbitraryExtensions: true });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const dts = result.read("consumer.d.ts");
        assert.match(dts, /export declare const result: number;/);
        assert.ok(result.exists("add.js"), "the imported .wat module should be emitted");
    });

    it("resolves .wat imports without listing them explicitly in the program", { skip }, () => {
        const result = compile(
            {
                "add.wat": addWat,
                "consumer.ts": `import { add } from "./add.wat";

export const result: number = add(1, 2);
`,
            },
            { allowArbitraryExtensions: true },
            { files: ["consumer.ts"] },
        );

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);
        assert.ok(result.exists("add.js"), "an import-only reference to a .wat file should include and emit it");
    });

    it("compiles .wat files without the DOM library using wasm_builtins", { skip }, () => {
        const result = compile({ "rich.wat": richWat }, { lib: ["esnext", "wasm_builtins"] });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const dts = result.read("rich.d.wat.ts");
        assert.match(dts, /export declare const memory: WebAssembly\.Memory;/);
    });

    it("reports a diagnostic when the WebAssembly text cannot be compiled", { skip }, () => {
        const result = compile({
            "bad.wat": `(module
  (func $f (result i32)
    i32.const)
)
`,
        });

        assert.notEqual(result.status, 0);
        assert.match(diagnostics(result), /wat2wasm failed|wasm-tools failed/);
    });

    it("emits runnable JavaScript that instantiates the compiled WebAssembly", { skip }, () => {
        const result = compile({ "add.wat": addWat });
        assert.equal(result.status, 0);

        const modulePath = path.join(result.outDir, "add.mjs");
        writeFileSync(modulePath, readFileSync(path.join(result.outDir, "add.js"), "utf8"));

        const executed = spawnSync(process.execPath, ["-e", `import(${JSON.stringify(modulePath)}).then((m) => { if (m.add(2, 3) !== 5) { process.exit(2); } })`], {
            encoding: "utf8",
        });
        assert.equal(executed.status, 0, `emitted module failed to execute:\n${executed.stdout}\n${executed.stderr}`);
    });
});
