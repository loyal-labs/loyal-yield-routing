export type StableMint = string;

export type SwapLane =
  | { kind: "same-mint" }
  | {
      kind: "loyal-hub";
      inputMint: StableMint;
      outputMint: StableMint;
      amountIn: bigint;
      minOut: bigint;
      maxFeeBps: number;
    }
  | {
      kind: "jupiter";
      inputMint: StableMint;
      outputMint: StableMint;
      amountIn: bigint;
      minOut: bigint;
    };

export type HubLiquidityQuote = {
  inputMint: StableMint;
  outputMint: StableMint;
  amountIn: bigint;
  minOut: bigint;
  maxFeeBps: number;
};

export type JupiterQuote = {
  inputMint: StableMint;
  outputMint: StableMint;
  amountIn: bigint;
  minOut: bigint;
};

export type StableSwapRouteInput = {
  inputMint: StableMint;
  outputMint: StableMint;
  amountIn: bigint;
  minOut: bigint;
  hub?: HubLiquidityQuote;
  jupiter?: JupiterQuote;
};

const ZERO = BigInt(0);

export function planStableSwapRoute(input: StableSwapRouteInput): SwapLane[] {
  if (input.amountIn <= ZERO) {
    throw new Error("amountIn must be positive");
  }

  if (input.inputMint === input.outputMint) {
    return [{ kind: "same-mint" }];
  }

  const lanes: SwapLane[] = [];
  const hubFill = planHubFill(input);
  if (hubFill !== null) {
    lanes.push(hubFill.lane);
  }

  const residualIn = input.amountIn - (hubFill?.amountIn ?? ZERO);
  if (residualIn > ZERO) {
    if (
      input.jupiter === undefined ||
      input.jupiter.inputMint !== input.inputMint ||
      input.jupiter.outputMint !== input.outputMint ||
      input.jupiter.amountIn !== residualIn
    ) {
      throw new Error("missing Jupiter residual quote");
    }
    lanes.push({
      kind: "jupiter",
      inputMint: input.inputMint,
      outputMint: input.outputMint,
      amountIn: input.jupiter.amountIn,
      minOut: input.jupiter.minOut,
    });
  }

  return lanes;
}

function planHubFill(
  input: StableSwapRouteInput,
): { amountIn: bigint; lane: SwapLane } | null {
  const hub = input.hub;
  if (
    hub === undefined ||
    hub.inputMint !== input.inputMint ||
    hub.outputMint !== input.outputMint ||
    hub.amountIn <= ZERO ||
    hub.minOut <= ZERO
  ) {
    return null;
  }

  if (hub.amountIn > input.amountIn) {
    throw new Error("hub fill cannot exceed route input amount");
  }

  return {
    amountIn: hub.amountIn,
    lane: {
      kind: "loyal-hub",
      inputMint: input.inputMint,
      outputMint: input.outputMint,
      amountIn: hub.amountIn,
      minOut: hub.minOut,
      maxFeeBps: hub.maxFeeBps,
    },
  };
}
