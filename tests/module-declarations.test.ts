import assert from "node:assert/strict";
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

before(() => {
    buildTsc();
});

describe("module declarations", () => {
    it("lowers an exported module declaration to an awaited Blob import", () => {
        const result = compile({
            "main.ts": `
export module Counter {
    let count = 0;
    export function increment(): number {
        return ++count;
    }
    export const label: string = "counter";
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /export const Counter = await import\(URL\.createObjectURL\(new Blob\(/);
        assert.match(js, /\[\s*`let count = 0;/);
        assert.match(js, /"application\/javascript"/);
        assert.ok(js.includes("export function increment()"), `expected the transformed module body in the Blob:\n${js}`);
        assert.ok(!js.includes(": number"), `type annotations should be erased in the Blob:\n${js}`);

        const dts = result.read("main.d.ts");
        assert.match(dts, /export declare namespace Counter \{/);
        assert.match(dts, /function increment\(\): number;/);
        assert.match(dts, /const label: string;/);
    });

    it("keeps non-exported module declarations local", () => {
        const result = compile({
            "main.ts": `
module helpers {
    export const base = 10;
    export function double(n: number): number {
        return n * 2;
    }
}

export const answer = helpers.double(helpers.base);
export type Helpers = typeof helpers;
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /const helpers = await import\(URL\.createObjectURL\(new Blob\(/);
        assert.ok(!js.includes("export const helpers"), `a non-exported declaration must not be exported:\n${js}`);
        assert.ok(js.includes("export const answer = helpers.double(helpers.base);"), js);

        const dts = result.read("main.d.ts");
        assert.match(dts, /declare namespace helpers \{/);
        assert.match(dts, /type Helpers = typeof helpers;/);
    });

    it("supports every static import form", () => {
        const result = compile({
            "main.ts": `
export module source {
    export const value = 1;
    export function compute(): number {
        return 2;
    }
    export default compute;
}

import defaultExport from source;
import { value, compute as renamed } from source;
import * as namespace from source;
import both, { value as aliased } from source;

export const combined = defaultExport() + value + renamed() + namespace.value + both() + aliased;
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /const \{ default: defaultExport \} = source;/);
        assert.match(js, /const \{ value, compute: renamed \} = source;/);
        assert.match(js, /const namespace = source;/);
        assert.match(js, /const \{ default: both, value: aliased \} = source;/);
        assert.ok(!js.includes("import "), `all imports should be lowered:\n${js}`);
    });

    it("lowers re-exports from a module declaration", () => {
        const result = compile({
            "main.ts": `
module source {
    export const a = 1;
    export const b = 2;
    export const c = 3;
    export default a;
}

export { a } from source;
export { b as renamed } from source;
export * from source;
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /const \{ a: \w+ \} = source;/);
        assert.match(js, /const \{ b: \w+ \} = source;/);
        assert.match(js, /export \{ \w+ as renamed \};/);
        // `export *` re-exports the remaining names (`b` and `c`) but never `default`, and skips `a`, which was
        // already exported explicitly.
        assert.match(js, /const \{ b: \w+, c: \w+ \} = source;/);
        assert.match(js, /export \{ \w+ as b, \w+ as c \};/);
        assert.equal((js.match(/ as a \};/g) ?? []).length, 1, `explicit \`a\` must only be exported once:\n${js}`);
        assert.ok(!js.includes("from source"), `no fragment from-clauses should remain:\n${js}`);
    });

    it("resolves imports declared inside the module body", () => {
        const result = compile({
            "dep.ts": `
export const base = 40;
export function bump(n: number): number {
    return n + 2;
}
`,
            "main.ts": `
export module consumer {
    import { base, bump } from "./dep.js";
    export const value = bump(base);
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.ok(js.includes('import { base, bump } from "./dep.js";'), `expected the relative import preserved inside the Blob:\n${js}`);

        const dts = result.read("main.d.ts");
        assert.match(dts, /export declare namespace consumer \{/);
        assert.match(dts, /const value: number;/);
    });

    it("lowers nested module declarations", () => {
        const result = compile({
            "main.ts": `
export module outer {
    export const a = 1;
    export module inner {
        export const b = 2;
    }
    export const c = inner.b;
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        const blobImports = js.match(/URL\.createObjectURL\(new Blob\(/g) ?? [];
        assert.equal(blobImports.length, 2, `expected two nested Blob imports:\n${js}`);
        assert.ok(js.includes("export const inner = await import"), `expected the inner declaration to be lowered:\n${js}`);
        assert.ok(js.includes("\\`export const b = 2;\\`"), `expected the inner template literal to be escaped:\n${js}`);

        const dts = result.read("main.d.ts");
        assert.match(dts, /export declare namespace outer \{/);
        assert.match(dts, /namespace inner \{/);
        assert.match(dts, /const b = 2;/);
    });

    it("exposes a module declaration as a type via typeof", () => {
        const result = compile({
            "main.ts": `
export module api {
    export const version: string = "1.0";
    export function ping(): void {}
}

export type Api = typeof api;
export const current: Api = api;
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const dts = result.read("main.d.ts");
        assert.match(dts, /export declare namespace api \{/);
        assert.match(dts, /type Api = typeof api;/);
        assert.match(dts, /export declare const current: Api;/);
    });

    it("erases TypeScript-only syntax inside the module body", () => {
        const result = compile({
            "main.ts": `
export module erased {
    export interface Shape { kind: string; }
    enum Direction { Up, Down }
    export const up: number = Direction.Up;
    export function describe(shape: Shape): string {
        return shape.kind;
    }
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.ok(!js.includes("interface Shape"), `interfaces should be erased:\n${js}`);
        assert.ok(!js.includes(": string"), `type annotations should be erased:\n${js}`);
        assert.ok(!js.includes("enum Direction"), `enums should be downleveled:\n${js}`);
        assert.ok(js.includes("Direction"), `enum references should remain:\n${js}`);

        const dts = result.read("main.d.ts");
        assert.match(dts, /interface Shape \{/);
        assert.match(dts, /const up: number;/);
        assert.match(dts, /function describe\(shape: Shape\): string;/);
    });

    it("does not treat a line break between `module` and the name as a declaration", () => {
        const result = compile({
            "main.ts": `export const before = 1;

module
NotAFragment {
    export const x = 1;
}
`,
        });

        const js = result.read("main.js");
        assert.ok(!js.includes("URL.createObjectURL"), `a line break must not start a module declaration:\n${js}`);
        assert.ok(js.includes("module;"), `expected the module identifier to remain:\n${js}`);
    });

    it("leaves namespace declarations untouched", () => {
        const result = compile({
            "main.ts": `
namespace Legacy {
    export const value = 1;
}

declare namespace Ambient {
    const secret: number;
}

export const legacyValue = Legacy.value;
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /var Legacy;/);
        assert.ok(!js.includes("URL.createObjectURL"), `namespaces must not become module declarations:\n${js}`);

        const dts = result.read("main.d.ts");
        assert.match(dts, /export declare const legacyValue = 1;/);
    });
});
