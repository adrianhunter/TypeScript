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

before(async () => {
    await buildTsc();
});

describe("zig (.zig) source files", () => {
    it("lowers functions and Zig types to TypeScript", () => {
        const result = compile({
            "main.zig": `
pub fn add(a: i32, b: i32) i32 {
    return a + b;
}

pub fn greet(name: []const u8) bool {
    return name.len > 0;
}

pub fn maybe(x: ?i32) i32 {
    return x orelse 0;
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /export function add\(a, b\) \{ return \(a \+ b\); \}/);
        assert.match(js, /name\.length > 0/);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export declare function add\(a: number, b: number\): number;/);
        assert.match(dts, /export declare function greet\(name: string\): boolean;/);
        assert.match(dts, /export declare function maybe\(x: \(number \| null\)\): number;/);
    });

    it("lowers a Zig struct to a module expression and a matching type", () => {
        const result = compile({
            "main.zig": `
pub const Point = struct {
    x: i32,
    y: i32,

    pub fn add(self: Point, other: Point) Point {
        return .{ .x = self.x + other.x, .y = self.y + other.y };
    }
};

pub fn origin() Point {
    return .{ .x = 0, .y = 0 };
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /export const Point = await import\(URL\.createObjectURL\(new Blob\(/);
        assert.match(js, /export function add\(self, other\)/);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export type Point = \{/);
        assert.match(dts, /x: number;/);
        assert.match(dts, /y: number;/);
        assert.match(dts, /export declare const Point: \{/);
        assert.match(dts, /add\(self: Point, other: Point\): Point;/);
        assert.match(dts, /export declare function origin\(\): Point;/);
    });

    it("lowers Zig enums to string unions and compares enum literals", () => {
        const result = compile({
            "main.zig": `
pub const Color = enum { red, green, blue };

pub fn isRed(color: Color) bool {
    return color == .red;
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /export const red = "red";/);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export type Color = "red" \| "green" \| "blue";/);
        assert.match(dts, /export declare const Color: \{/);
        assert.match(dts, /export declare function isRed\(color: Color\): boolean;/);
    });

    it("lowers unions with payload captures and switch expressions", () => {
        const result = compile({
            "main.zig": `
pub const Shape = union(enum) {
    circle: f64,
    square: f64,

    pub fn area(self: Shape) f64 {
        return switch (self) {
            .circle => |r| 3.0 * r * r,
            .square => |s| s * s,
        };
    }
};
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export type Shape = \{/);
        assert.match(dts, /circle: number;/);
        assert.match(dts, /area\(self: Shape\): number;/);
    });

    it("lowers test blocks to exported functions", () => {
        const result = compile({
            "main.zig": `
test "addition works" {
    const x = 1 + 1;
    _ = x;
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /export function test_addition_works\(\)/);
        assert.match(js, /const x = \(1 \+ 1\);/);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export declare function test_addition_works\(\): void;/);
    });

    it("handles Zig expressions, ranges, and multiline strings", () => {
        const result = compile({
            "main.zig": `
pub fn sign(n: i32) i32 {
    return if (n > 0) 1 else if (n < 0) -1 else 0;
}

pub fn total(n: i32) i32 {
    var sum: i32 = 0;
    for (0..n) |i| {
        sum += i;
    }
    return sum;
}

pub fn text() []const u8 {
    return
        \\\\hello
        \\\\world
    ;
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /Array\.from\(\{ length: \(n\) - \(0\) \}/);
        assert.match(js, /return "hello\\nworld";/);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export declare function sign\(n: number\): number;/);
        assert.match(dts, /export declare function total\(n: number\): number;/);
        assert.match(dts, /export declare function text\(\): string;/);
    });

    it("lowers builtins used in type position", () => {
        const result = compile({
            "main.zig": `
pub const Options = struct {
    logFn: fn (
        comptime message_level: u32,
        comptime scope: @EnumLiteral(),
        comptime format: []const u8,
        args: anytype,
    ) void,
};
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /logFn: \(message_level: number, scope: string, format: string, args: any\) => void;/);
    });

    it("lowers anonymous tuple literals to arrays", () => {
        const result = compile({
            "main.zig": `
pub fn format(fmt: []const u8, args: anytype) void {
    _ = fmt;
    _ = args;
}

pub fn main() void {
    format("hi {s}", .{"codebase"});
    const point = .{ 1, 2 };
    _ = point;
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /format\("hi \{s\}", \["codebase"\]\);/);
        assert.match(js, /const point = \[1, 2\];/);
    });

    it("lowers switch statements and expressions including else prongs", () => {
        const result = compile({
            "main.zig": `
pub fn label(n: i32) []const u8 {
    return switch (n) {
        0 => "zero",
        1, 2 => "small",
        else => "other",
    };
}

pub fn describe(n: i32) []const u8 {
    switch (n) {
        0 => {
            return "zero";
        },
        else => {
            return "other";
        },
    }
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /default:/);
        assert.match(js, /return "other";/);
        assert.ok(js.includes('"small"'), js);
        assert.match(js, /switch \(/);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export declare function label\(n: number\): string;/);
        assert.match(dts, /export declare function describe\(n: number\): string;/);
    });

    it("handles @hasDecl, inline @import, and @import with a .zig suffix", () => {
        const result = compile({
            "dep.zig": `
pub const value = 42;
`,
            "main.zig": `
const dep = @import("dep.zig");

pub fn has(opts: anytype) bool {
    return @hasDecl(opts, "field");
}

pub fn isDebug() bool {
    return @import("builtin").mode == .debug;
}

pub fn get() i32 {
    return dep.value;
}
`,
        }, { allowArbitraryExtensions: true });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /import \* as dep from "\.\/dep\.zig";/);
        assert.match(js, /"field" in opts/);
        assert.match(js, /globalThis\.mode === "debug"/);
        assert.match(js, /dep\.value/);
    });

    it("supports function types, optional captures, and sentinel slices", () => {
        const result = compile({
            "main.zig": `
pub const Callback = fn (x: i32) i32;

pub fn unwrap(x: ?i32) i32 {
    if (x) |v| {
        return v;
    }
    return 0;
}

pub fn names(items: [][:0]const u8) []const u8 {
    return items[0];
}
`,
        });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /if \(x != null\)/);
        assert.match(js, /const v = x;/);
        assert.match(js, /return v;/);

        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export type Callback = \(x: number\) => number;/);
        assert.match(dts, /export declare function unwrap\(x: \(number \| null\)\): number;/);
        assert.match(dts, /export declare function names\(items: string\[\]\): string;/);
    });

    it("resolves imports between Zig modules with allowArbitraryExtensions", () => {
        const result = compile({
            "math.zig": `
pub fn square(x: i32) i32 {
    return x * x;
}
`,
            "main.zig": `
const math = @import("./math");

pub fn compute() i32 {
    return math.square(3);
}
`,
        }, { allowArbitraryExtensions: true });

        assert.equal(diagnostics(result), "");
        assert.equal(result.status, 0);

        const js = result.read("main.js");
        assert.match(js, /import \* as math from "\.\/math";/);
        assert.match(js, /math\.square\(3\)/);

        assert.ok(result.exists("math.js"), "the imported .zig module should be emitted");
        const dts = result.read("main.d.zig.ts");
        assert.match(dts, /export declare function compute\(\): number;/);
    });
});
