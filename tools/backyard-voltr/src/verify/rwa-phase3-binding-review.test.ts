import { expect, test } from "bun:test";
import { accountVectorDifferences } from "./rwa-phase3-binding-review.js";

test("policy account review is positional and rejects missing or unconstrained accounts",()=>{
  const constraints=[{index:0,pubkeys:["vault"]},{index:1,pubkeys:["destination"]}];
  expect(accountVectorDifferences(["vault","destination"],constraints)).toEqual([]);
  expect(accountVectorDifferences(["destination","vault"],constraints)).toHaveLength(2);
  expect(accountVectorDifferences(["vault"],constraints)).toEqual([{index:1,actual:null,allowed:["destination"]}]);
  expect(accountVectorDifferences(["vault","destination","extra"],constraints)).toEqual([{index:2,actual:"extra",allowed:[]}]);
  expect(()=>accountVectorDifferences(["vault"],[constraints[0]!,constraints[0]!])).toThrow("INVALID_CONSTRAINT_INDICES");
});
