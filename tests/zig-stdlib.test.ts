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
import path from "node:path";
import {
    before,
    describe,
    it,
} from "node:test";
import {
    buildTsc,
    repoRoot,
    tscBin,
    tscDir,
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
        }
        else if (entry.endsWith(".zig")) {
            files.push(full);
        }
    }
    return files;
}

function commandExists(command: string): boolean {
    return spawnSync("bash", ["-lc", `command -v ${command}`], { encoding: "utf8" }).status === 0;
}

/**
 * Runs the compile inside the default OrbStack Linux machine when available (a 8GB VM), enforcing a
 * soft RSS cap so a regression cannot throw the machine into swap. Falls back to running the host
 * binary directly otherwise.
 */
function runCompile(tsconfig: string, tmpDir: string): { status: number; output: string; peakRssMb: number; } {
    const useOrb = process.platform === "darwin" && commandExists("orb");
    if (!useOrb) {
        const result = spawnSync(tscBin, ["-p", tsconfig, "--pretty", "false"], { encoding: "utf8" });
        return { status: result.status ?? -1, output: `${result.stdout}${result.stderr}`.trim(), peakRssMb: -1 };
    }

    const goarch = process.arch === "arm64" ? "arm64" : "amd64";
    const linuxBin = path.join(tmpDir, "tsc.linux");
    const built = spawnSync("go", ["build", "-o", linuxBin, "./cmd/tsc"], {
        cwd: tscDir,
        encoding: "utf8",
        env: { ...process.env, GOOS: "linux", GOARCH: goarch, CGO_ENABLED: "0" },
    });
    if (built.status !== 0) {
        throw new Error(`Failed to build Linux tsc:\n${built.stdout}\n${built.stderr}`);
    }

    const monitor = path.join(tmpDir, "monitor.sh");
    const log = path.join(tmpDir, "tsc-output.log");
    writeFileSync(
        monitor,
        `#!/usr/bin/env bash
set -u
cap_mb=\${CAP_MB:-6500}
log="${log}"
"$@" >"$log" 2>&1 &
pid=$!
peak=0
killed=0
while kill -0 "$pid" 2>/dev/null; do
    hwm=$(awk '/VmHWM/{print $2}' "/proc/$pid/status" 2>/dev/null)
    rss=$(awk '/VmRSS/{print $2}' "/proc/$pid/status" 2>/dev/null)
    for value in "\${hwm:-}" "\${rss:-}"; do
        if [ -n "$value" ] && [ "$value" -gt "$peak" ]; then peak=$value; fi
    done
    if [ -n "\${rss:-}" ] && [ "$((rss / 1024))" -gt "$cap_mb" ]; then
        kill -9 "$pid" 2>/dev/null
        killed=1
        break
    fi
    sleep 0.05
done
wait "$pid"
status=$?
echo "TSC_STATUS=$status PEAK_RSS_MB=$((peak / 1024)) KILLED=$killed"
cat "$log"
`,
    );

    const result = spawnSync("orb", ["bash", monitor, linuxBin, "-p", tsconfig, "--pretty", "false"], {
        encoding: "utf8",
    });
    const combined = `${result.stdout}${result.stderr}`;
    const header = /TSC_STATUS=(-?\d+) PEAK_RSS_MB=(\d+) KILLED=(\d+)/.exec(combined);
    assert.ok(header, `missing monitor header in output:\n${combined}`);
    assert.equal(header[3], "0", "compiler exceeded the memory cap and was killed");
    return {
        status: Number(header[1]),
        output: combined.replace(header[0], "").trim(),
        peakRssMb: Number(header[2]),
    };
}

before(async () => {
    await buildTsc();
});

describe("zig std library", () => {
    it("type-checks every .zig file in the standard library", t => {
        if (!existsSync(stdlibDir)) {
            t.skip(`ZIG_STDLIB not found at ${stdlibDir}`);
            return;
        }

        const files = collectZigFiles(stdlibDir).sort();
        assert.ok(files.length > 0, "expected to find .zig files in the standard library");

        const dir = mkdtempSync(path.join(repoRoot, ".zig-stdlib-"));
        const tsconfig = path.join(dir, "tsconfig.json");
        writeFileSync(
            tsconfig,
            JSON.stringify({
                compilerOptions: {
                    target: "esnext",
                    module: "esnext",
                    moduleResolution: "bundler",
                    allowArbitraryExtensions: true,
                    // Each Zig file is its own module; without this, every file shares one global scope.
                    moduleDetection: "force",
                    // Full type checking, including `strict`.
                    strict: true,
                    skipLibCheck: true,
                    noEmit: true,
                    types: [],
                },
                files,
            }),
        );

        try {
            const result = runCompile(tsconfig, dir);
            assert.equal(result.output, "", `expected no diagnostics, got:\n${result.output}`);
            assert.equal(result.status, 0, `tsc exited with status ${result.status}`);
            if (result.peakRssMb >= 0) {
                assert.ok(result.peakRssMb < 7000, `peak RSS ${result.peakRssMb}MB exceeded the 8GB VM budget`);
            }
        }
        finally {
            rmSync(dir, { recursive: true, force: true });
        }
    });
});
