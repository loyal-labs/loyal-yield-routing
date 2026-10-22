import { expect, test } from "bun:test";
import { readCompare } from "./find-drift-candidates";

test("readCompare keeps files and commits from every page", () => {
  const pages = [
    {
      merge_base_commit: { sha: "mb" },
      files: [{ filename: "a.ts" }, { filename: "b.ts" }],
      commits: [{ sha: "c1", commit: { message: "one" }, author: { login: "x" } }],
    },
    {
      files: [{ filename: "c.ts" }],
      commits: [{ sha: "c2", commit: { message: "two" }, author: null }],
    },
  ];
  const result = readCompare((page) => pages[page - 1] ?? {}, 2);
  expect(result.files).toEqual(["a.ts", "b.ts", "c.ts"]);
  expect(result.commits.map((c) => c.sha)).toEqual(["c1", "c2"]);
  expect(result.mergeBaseSha).toBe("mb");
});
