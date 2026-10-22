export function isVoltrOperationVisible(
  signature: string,
  summary: { observedSlot?: string } | null,
  operations: readonly {
    confirmedSlot?: string;
    signature: string | null;
    status: string;
  }[]
): boolean {
  const observedSlot = summary?.observedSlot;
  if (!observedSlot || !/^\d+$/.test(observedSlot)) return false;
  return operations.some((operation) => {
    const slot = operation.confirmedSlot;
    return (
      operation.signature === signature &&
      operation.status === "confirmed" &&
      Boolean(
        slot && /^\d+$/.test(slot) && BigInt(observedSlot) >= BigInt(slot)
      )
    );
  });
}
