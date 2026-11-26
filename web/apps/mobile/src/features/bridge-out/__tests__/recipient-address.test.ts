import { bridgeRecipientState } from "../recipient-address";

const evm = "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359";
const tron = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb";

test("accepts lowercase or EIP-55 EVM recipients and rejects a mistyped checksum", () => {
  expect(bridgeRecipientState("evm", evm)).toBe("valid");
  expect(bridgeRecipientState("evm", evm.toLowerCase())).toBe("valid");
  expect(bridgeRecipientState("evm", evm.replace("bB79", "bb79"))).toBe(
    "invalid",
  );
  expect(bridgeRecipientState("evm", evm.replace("0x", "0X"))).toBe("invalid");
});

test("requires the Tron network byte and the full Base58Check checksum", () => {
  expect(bridgeRecipientState("tron", tron)).toBe("valid");
  expect(bridgeRecipientState("tron", tron.slice(0, -1) + "c")).toBe("invalid");
  expect(
    bridgeRecipientState("tron", "1BoatSLRHtKNngkdXEeobR76b53LETtpyT"),
  ).toBe("invalid");
});

test("keeps a typed prefix out of exact matches and rejects it when submitted", () => {
  for (const [kind, prefix] of [
    ["evm", "0xfB69"],
    ["tron", "T9yD"],
  ] as const) {
    expect(bridgeRecipientState(kind, prefix)).toBe("partial");
    expect(bridgeRecipientState(kind, prefix, true)).toBe("invalid");
    expect(bridgeRecipientState(kind, "")).toBe("empty");
  }
});
