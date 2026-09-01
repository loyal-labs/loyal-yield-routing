export type DownstreamRollbackLogProof = Readonly<{
  armInvokeLogIndex: number;
  armSuccessLogIndex: number;
  downstreamInvokeLogIndex: number;
  downstreamFailureLogIndex: number;
}>;

export const DOWNSTREAM_ROLLBACK_MUTATION = "voltr_failure_rolls_back_ticket_and_capital";

export function failedSimulationOverlayAccepted(input: Readonly<{
  mutationName: string;
  inspectedAddresses: readonly string[];
  postAccountsAvailable: boolean | null;
  nullAddresses: readonly string[];
  changedAddresses: readonly string[];
  downstreamRollbackProven: boolean;
}>): boolean {
  const isDownstreamRollback = input.mutationName === DOWNSTREAM_ROLLBACK_MUTATION;
  if (isDownstreamRollback && !input.downstreamRollbackProven) return false;
  const concreteUnchanged = input.postAccountsAvailable === true
    && input.nullAddresses.length === 0
    && input.changedAddresses.length === 0;
  if (concreteUnchanged) return true;
  if (!isDownstreamRollback
    || input.postAccountsAvailable !== false
    || input.changedAddresses.length !== 0
    || input.nullAddresses.length !== input.inspectedAddresses.length) return false;
  const expected = new Set(input.inspectedAddresses);
  return expected.size === input.inspectedAddresses.length
    && new Set(input.nullAddresses).size === input.nullAddresses.length
    && input.nullAddresses.every((address) => expected.has(address));
}

export function downstreamRollbackLogProof(
  logs: readonly string[],
  armProgram: string,
  downstreamProgram: string,
): DownstreamRollbackLogProof | null {
  const armInvokeLogIndex = logs.findIndex((line) => line.startsWith(`Program ${armProgram} invoke [`));
  const armSuccessLogIndex = logs.findIndex((line, index) => index > armInvokeLogIndex
    && line === `Program ${armProgram} success`);
  const downstreamInvokeLogIndex = logs.findIndex((line, index) => index > armSuccessLogIndex
    && line.startsWith(`Program ${downstreamProgram} invoke [`));
  const downstreamFailureLogIndex = logs.findIndex((line, index) => index > downstreamInvokeLogIndex
    && line.startsWith(`Program ${downstreamProgram} failed:`));
  return armInvokeLogIndex >= 0
    && armSuccessLogIndex > armInvokeLogIndex
    && downstreamInvokeLogIndex > armSuccessLogIndex
    && downstreamFailureLogIndex > downstreamInvokeLogIndex
    ? { armInvokeLogIndex, armSuccessLogIndex, downstreamInvokeLogIndex, downstreamFailureLogIndex }
    : null;
}
