import type { BridgeDestination } from "@loyal-labs/api";
import bs58 from "bs58";
import { isAddress, sha256 } from "viem/utils";

export function bridgeRecipientState(
  kind: BridgeDestination["addressKind"],
  address: string,
  complete = false,
): "empty" | "partial" | "invalid" | "valid" {
  if (!address) return "empty";
  if (kind === "evm" && isAddress(address, { strict: true })) return "valid";
  if (kind === "tron" && address.length === 34) {
    const bytes = bs58.decodeUnsafe(address);
    if (bytes?.length === 25 && bytes[0] === 0x41) {
      const checksum = sha256(sha256(bytes.subarray(0, 21), "bytes"), "bytes");
      if (bytes.subarray(21).every((byte, i) => byte === checksum[i]))
        return "valid";
    }
  }
  return complete ||
    address.length >= (kind === "tron" ? 34 : 42) ||
    /[^a-z\d]/i.test(address)
    ? "invalid"
    : "partial";
}
