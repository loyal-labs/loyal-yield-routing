import { afterEach, beforeEach, expect, test } from "bun:test";

import {
  clearPendingCode,
  readPendingCode,
  storeLinkCode,
  withPendingLinkRef,
} from "./pending-code";

const jar = new Map<string, string>();
const written: string[] = [];

beforeEach(() => {
  jar.clear();
  written.length = 0;
  Object.assign(globalThis, {
    window: { location: { hostname: "next.askloyal.com", protocol: "https:" } },
    document: {
      get cookie() {
        return [...jar].map(([k, v]) => `${k}=${v}`).join("; ");
      },
      set cookie(line: string) {
        written.push(line);
        const [pair, ...attrs] = line.split("; ");
        const [name, ...rest] = pair.split("=");
        if (attrs.includes("Max-Age=0")) jar.delete(name);
        else jar.set(name, rest.join("="));
      },
    },
  });
});

afterEach(() => {
  Reflect.deleteProperty(globalThis, "window");
  Reflect.deleteProperty(globalThis, "document");
});

test("a link code is kept for a visitor with no flags and rides the wallet browse link", () => {
  expect(storeLinkCode("a11ce00")).toBe(true);
  expect(readPendingCode()).toEqual({ code: "A11CE00", source: "link" });
  expect(
    new URL(
      withPendingLinkRef("https://next.askloyal.com/app")
    ).searchParams.get("ref")
  ).toBe("A11CE00");
  expect(written[0]).toContain("Domain=askloyal.com");

  clearPendingCode();
  expect(readPendingCode()).toBeNull();
});
