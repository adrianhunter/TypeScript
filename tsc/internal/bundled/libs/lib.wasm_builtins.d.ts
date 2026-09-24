/*! *****************************************************************************
Copyright (c) Microsoft Corporation. All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at http://www.apache.org/licenses/LICENSE-2.0

THIS CODE IS PROVIDED ON AN *AS IS* BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, EITHER EXPRESS OR IMPLIED, INCLUDING WITHOUT LIMITATION ANY IMPLIED
WARRANTIES OR CONDITIONS OF TITLE, FITNESS FOR A PARTICULAR PURPOSE,
MERCHANTABILITY OR NON-INFRINGEMENT.

See the Apache Version 2.0 License for specific language governing permissions
and limitations under the License.
***************************************************************************** */


/**
 * Minimal declarations for the WebAssembly JavaScript API, used by `.wat` source files that are compiled to
 * WebAssembly and re-exported as JavaScript modules.
 *
 * These declarations are intentionally self-contained so that they can be used without the DOM library (for
 * example with `"lib": ["esnext", "wasm_builtins"]`). Do not include this library together with `lib.dom.d.ts`
 * or `lib.webworker.d.ts`, which already declare the `WebAssembly` namespace.
 */
declare namespace WebAssembly {
    interface WebAssemblyInstantiatedSource {
        module: Module;
        instance: Instance;
    }

    interface Module {}

    interface Instance {
        readonly exports: Exports;
    }

    interface Memory {
        readonly buffer: ArrayBuffer;
        grow(delta: number): number;
    }

    interface Table {
        readonly length: number;
        get(index: number): any;
        set(index: number, value: any): void;
        grow(delta: number): number;
    }

    interface Global<T = any> {
        value: T;
        valueOf(): T;
    }

    interface Exports {
        [name: string]: any;
    }

    interface Exception {}
    interface Tag {}

    interface CompileError extends Error {}
    interface LinkError extends Error {}
    interface RuntimeError extends Error {}

    function compile(bytes: ArrayBuffer | ArrayBufferView): Promise<Module>;
    function compileStreaming(source: Promise<any>): Promise<Module>;
    function instantiate(bytes: ArrayBuffer | ArrayBufferView, importObject?: any): Promise<WebAssemblyInstantiatedSource>;
    function instantiate(moduleObject: Module, importObject?: any): Promise<Instance>;
    function instantiateStreaming(source: Promise<any>, importObject?: any): Promise<WebAssemblyInstantiatedSource>;
    function validate(bytes: ArrayBuffer | ArrayBufferView): boolean;

    var Module: {
        new (bytes: ArrayBuffer | ArrayBufferView): Module;
        prototype: Module;
    };
    var Instance: {
        new (module: Module, importObject?: any): Instance;
        prototype: Instance;
    };
    var Memory: {
        new (descriptor: { initial: number; maximum?: number; shared?: boolean }): Memory;
        prototype: Memory;
    };
    var Table: {
        new (descriptor: { element: string; initial: number; maximum?: number }): Table;
        prototype: Table;
    };
    var Global: {
        new (descriptor: { value: any; mutable?: boolean }, value?: any): Global;
        prototype: Global;
    };
}
