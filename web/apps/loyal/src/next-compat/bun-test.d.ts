// The scratch tests next to this file (*.scratch.test.ts, git-ignored) run
// with `bun test`; these are the parts they import. Bun's own types aren't
// installed and must not be, so the runner surface is declared here.
declare module "bun:test" {
  export function describe(name: string, fn: () => void): void;
  export function test(name: string, fn: () => void): void;
  export function expect(value: unknown): {
    toBe(expected: unknown): void;
    toEqual(expected: unknown): void;
    toBeInstanceOf(expected: unknown): void;
  };
}
