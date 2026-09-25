import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
    existsSync,
    mkdtempSync,
    readdirSync,
    rmSync,
    statSync,
    writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import {
    before,
    describe,
    it,
} from "node:test";
import {
    buildTsc,
    tscBin,
} from "./helpers.ts";

/**
 * Location of the Zig standard library to exercise. Overridable through the
 * `ZIG_STDLIB` environment variable; the test is skipped when it is absent.
 */
const stdlibDir = process.env.ZIG_STDLIB ?? "/Users/boo/.zvm/master/lib/std";

function collectZigFiles(dir: string): string[] {
    const files: string[] = [];
    for (const entry of readdirSync(dir)) {
        const full = path.join(dir, entry);
        if (statSync(full).isDirectory()) {
            files.push(...collectZigFiles(full));
        } else if (entry.endsWith(".zig")) {
            files.push(full);
        }
    }
    return files;
}

before(async () => {
    await buildTsc();
});

describe("zig std library", () => {
    it("compiles every .zig file in the standard library", (t) => {
        if (!existsSync(stdlibDir)) {
            t.skip(`ZIG_STDLIB not found at ${stdlibDir}`);
            return;
        }

        const files = collectZigFiles(stdlibDir).sort();
        assert.ok(files.length > 0, "expected to find .zig files in the standard library");

        const dir = mkdtempSync(path.join(tmpdir(), "ts-zig-stdlib-"));
        const tsconfig = path.join(dir, "tsconfig.json");
        writeFileSync(tsconfig, JSON.stringify({
            compilerOptions: {
                target: "esnext",
                module: "esnext",
                moduleResolution: "bundler",
                allowArbitraryExtensions: true,
                // The Zig front end lowers to loosely typed TypeScript, so only
                // synthesize and parse the output; full type checking is not the
                // goal here.
                noCheck: true,
                skipLibCheck: true,
                noEmit: true,
            },
            files,
        }));

        const result = spawnSync(tscBin, ["-p", tsconfig, "--pretty", "false"], { encoding: "utf8" });
        const diags = `${result.stdout}${result.stderr}`.trim();
        rmSync(dir, { recursive: true, force: true });

        assert.equal(diags, "");
        assert.equal(result.status, 0);
    });
});
