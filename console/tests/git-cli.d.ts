// The types of tests/git-cli.js, for tests/git.test.ts.

export function cleanUp(): void;
export function dir(): string;
export function git(cwd: string, ...args: string[]): string;
export function gitBytes(cwd: string, ...args: string[]): Uint8Array;
export function gitIn(cwd: string, input: Uint8Array, ...args: string[]): Uint8Array;
export function write(path: string, content: string, executable?: boolean): void;
export function joined(...parts: string[]): string;
export function zlib(data: Uint8Array, level: number): Uint8Array;
export function rawDeflate(data: Uint8Array): Uint8Array;
