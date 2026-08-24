import { createHmac } from "node:crypto";
import { access, readFile, writeFile } from "node:fs/promises";
type SetupStage =
  | "approve_token_delegate"
  | "close_autodeposit"
  | "create_policy"
  | "create_recurring_delegation"
  | "initialize_subscription_authority";

type LocalState = {
  delegatedSigner: string;
  policyAccount: string;
  policySeed: string;
  recurringDelegation: string;
  settingsPda: string;
  subscriptionAuthority: string;
  vaultPubkey: string;
  vaultUsdcAta: string;
  walletAddress: string;
  walletBalanceFloorRaw: string;
  walletUsdcAta: string;
};

type RecordedTransaction = {
  signature: string;
  stage: SetupStage;
};

type Args = {
  authSecret?: string;
  closeOutput?: string;
  closeReady?: string;
  eventsUrl?: string;
  expectedReason?: string;
  expectedUiState?: "created" | "deleted";
  output: string;
  pendingFloorReady?: string;
  postgresUrl?: string;
  rpcUrl?: string;
  state?: string;
  treasury?: string;
};

const loyalAppRoot = process.env.LOYAL_APP_ROOT;
if (!loyalAppRoot) {
  throw new Error(
    "LOYAL_APP_ROOT is required as a read-only dependency source."
  );
}
const { createSmartAccountVaultsClient } = await import(
  `${loyalAppRoot}/packages/smart-account-vaults/src/index.ts`
);
const {
  codecs,
  createLoyalSmartAccountsClient,
  pda,
  PROGRAM_ID,
  smartAccounts,
} = await import(`${loyalAppRoot}/packages/loyal-smart-accounts/src/index.ts`);
const { LoyalCluster } = await import(
  `${loyalAppRoot}/packages/loyal-actions/src/index.ts`
);
const {
  createAssociatedTokenAccountIdempotentInstruction,
  getAccount,
  getAssociatedTokenAddressSync,
  TOKEN_PROGRAM_ID,
} = await import(
  `${loyalAppRoot}/node_modules/@solana/spl-token/lib/cjs/index.js`
);
const {
  Connection,
  Keypair,
  LAMPORTS_PER_SOL,
  PublicKey,
  sendAndConfirmTransaction,
  Transaction,
  TransactionMessage,
} = await import(
  `${loyalAppRoot}/node_modules/@solana/web3.js/lib/index.cjs.js`
);

type LocalConnection = InstanceType<typeof Connection>;
type LocalPublicKey = InstanceType<typeof PublicKey>;

function parseArgs(argv: string[]): { command: string; args: Args } {
  const [command, ...rest] = argv;
  if (!command || !["listen", "setup"].includes(command)) {
    throw new Error("Expected setup or listen command.");
  }
  const values: Record<string, string> = {};
  for (let index = 0; index < rest.length; index += 2) {
    const key = rest[index];
    const value = rest[index + 1];
    if (!(key?.startsWith("--") && value)) {
      throw new Error(`Invalid argument near ${key ?? "end of command"}.`);
    }
    values[key.slice(2)] = value;
  }
  if (!values.output) {
    throw new Error("--output is required.");
  }
  return {
    command,
    args: {
      authSecret: values["auth-secret"],
      closeOutput: values["close-output"],
      closeReady: values["close-ready"],
      eventsUrl: values["events-url"],
      expectedReason: values["expected-reason"],
      expectedUiState: values["expected-ui-state"] as
        | "created"
        | "deleted"
        | undefined,
      output: values.output,
      pendingFloorReady: values["pending-floor-ready"],
      postgresUrl: values["postgres-url"],
      rpcUrl: values["rpc-url"],
      state: values.state,
      treasury: values.treasury,
    },
  };
}

async function waitForFinalized(
  connection: LocalConnection,
  signature: string
): Promise<void> {
  for (let attempt = 0; attempt < 300; attempt += 1) {
    const status = (
      await connection.getSignatureStatuses([signature], {
        searchTransactionHistory: true,
      })
    ).value[0];
    if (status?.err) {
      throw new Error(
        `Transaction ${signature} failed: ${JSON.stringify(status.err)}`
      );
    }
    if (status?.confirmationStatus === "finalized") {
      return;
    }
    await Bun.sleep(100);
  }
  throw new Error(`Transaction ${signature} did not finalize.`);
}

async function writeChainTransactions(args: {
  connection: LocalConnection;
  output: string;
  transactions: RecordedTransaction[];
}) {
  const records = [];
  for (const transaction of args.transactions) {
    await waitForFinalized(args.connection, transaction.signature);
    const response = await args.connection.getTransaction(
      transaction.signature,
      {
        commitment: "finalized",
        maxSupportedTransactionVersion: 0,
      }
    );
    if (!response) {
      throw new Error(
        `Finalized transaction ${transaction.signature} was not found.`
      );
    }
    if (response.meta?.err) {
      throw new Error(
        `Finalized transaction ${
          transaction.signature
        } failed: ${JSON.stringify(response.meta.err)}`
      );
    }
    const message = response.transaction.message;
    if (message.addressTableLookups.length !== 0) {
      throw new Error(
        "Local Autodeposit transaction unexpectedly used an ALT."
      );
    }
    const decompiled = TransactionMessage.decompile(message);
    records.push({
      instructions: decompiled.instructions.map((instruction) => ({
        accounts: instruction.keys.map((account) => ({
          isSigner: account.isSigner,
          isWritable: account.isWritable,
          pubkey: account.pubkey.toBase58(),
        })),
        data: Buffer.from(instruction.data).toString("base64"),
        programId: instruction.programId.toBase58(),
      })),
      signature: transaction.signature,
      slot: response.slot,
      stage: transaction.stage,
    });
  }
  await writeFile(
    args.output,
    `${records.map((record) => JSON.stringify(record)).join("\n")}\n`
  );
}

async function createLocalSmartAccount(
  connection: LocalConnection,
  treasury: LocalPublicKey
) {
  const wallet = Keypair.generate();
  const delegatedSigner = Keypair.generate();
  const airdrop = await connection.requestAirdrop(
    wallet.publicKey,
    20 * LAMPORTS_PER_SOL
  );
  await waitForFinalized(connection, airdrop);

  const client = createLoyalSmartAccountsClient({
    connection,
    defaultCommitment: "confirmed",
    programId: PROGRAM_ID,
  });
  const [programConfigPda] = pda.getProgramConfigPda({ programId: PROGRAM_ID });
  if (!(await connection.getAccountInfo(programConfigPda, "finalized"))) {
    throw new Error(
      "Local validator is missing the Squads ProgramConfig genesis account."
    );
  }
  const [settingsPda] = pda.getSettingsPda({
    accountIndex: BigInt(1),
    programId: PROGRAM_ID,
  });
  const prepared = await smartAccounts.prepare.create({
    creator: wallet.publicKey,
    programId: PROGRAM_ID,
    rentCollector: null,
    settings: settingsPda,
    settingsAuthority: null,
    signers: [
      {
        key: wallet.publicKey,
        permissions: codecs.Permissions.all(),
      },
    ],
    threshold: 1,
    timeLock: 0,
    treasury,
  });
  await client.send(prepared, { confirm: true, signers: [wallet] });
  return { delegatedSigner, settingsPda, wallet };
}

async function setup(args: Args) {
  if (!(args.rpcUrl && args.treasury)) {
    throw new Error("setup requires --rpc-url and --treasury.");
  }
  if (Boolean(args.closeReady) !== Boolean(args.closeOutput)) {
    throw new Error("setup requires both --close-ready and --close-output.");
  }
  const connection = new Connection(args.rpcUrl, "confirmed");
  const { delegatedSigner, settingsPda, wallet } =
    await createLocalSmartAccount(connection, new PublicKey(args.treasury));
  const vaults = createSmartAccountVaultsClient({
    connection,
    programId: PROGRAM_ID,
  });
  const usdcMint = new PublicKey(
    "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
  );
  const walletUsdcAta = getAssociatedTokenAddressSync(
    usdcMint,
    wallet.publicKey,
    false,
    TOKEN_PROGRAM_ID
  );
  await sendAndConfirmTransaction(
    connection,
    new Transaction().add(
      createAssociatedTokenAccountIdempotentInstruction(
        wallet.publicKey,
        walletUsdcAta,
        wallet.publicKey,
        usdcMint,
        TOKEN_PROGRAM_ID
      )
    ),
    [wallet],
    { commitment: "finalized" }
  );

  const transactions: RecordedTransaction[] = [];
  const walletBalanceFloorRaw = BigInt(2_000_000);
  const nonce = BigInt(42);
  let policySeed: bigint | undefined;
  let completedState: LocalState | null = null;
  for (let round = 0; round < 4; round += 1) {
    const stages = await vaults.prepareEarnUsdcAutodepositSetupBatch({
      amountRaw: BigInt(1_000_000),
      cluster: LoyalCluster.MainnetBeta,
      expiryTimestamp: BigInt(0),
      feePayer: wallet.publicKey,
      minimumDelegatorBalanceRaw: walletBalanceFloorRaw,
      nonce,
      periodLengthSeconds: BigInt(3600),
      policySeed,
      policySigner: delegatedSigner.publicKey,
      settingsPda,
      signer: wallet.publicKey,
      walletAddress: wallet.publicKey,
    });
    if (stages.length === 0) {
      throw new Error("Web Autodeposit setup prepared no stages.");
    }
    for (const stage of stages) {
      const setupStage = stage.stage as SetupStage;
      const signature = await vaults.sdk.send(stage.prepared, {
        confirm: true,
        signers: [wallet],
      });
      transactions.push({ signature, stage: setupStage });
      if (setupStage !== "initialize_subscription_authority") {
        const nextPolicySeed =
          stage.policy.seed ?? stage.persistence.policySeed;
        if (nextPolicySeed === null) {
          throw new Error(`Autodeposit ${setupStage} omitted the policy seed.`);
        }
        policySeed = BigInt(nextPolicySeed);
      }
      if (
        setupStage === "create_recurring_delegation" ||
        setupStage === "approve_token_delegate"
      ) {
        const { policyAccount, policySeed: persistedPolicySeed } =
          stage.persistence;
        if (policyAccount === null || persistedPolicySeed === null) {
          throw new Error(
            `Autodeposit ${setupStage} omitted persisted policy identity.`
          );
        }
        completedState = {
          delegatedSigner: delegatedSigner.publicKey.toBase58(),
          policyAccount,
          policySeed: persistedPolicySeed,
          recurringDelegation: stage.persistence.recurringDelegation,
          settingsPda: settingsPda.toBase58(),
          subscriptionAuthority: stage.persistence.subscriptionAuthority,
          vaultPubkey: stage.persistence.vaultPubkey,
          vaultUsdcAta: stage.persistence.vaultUsdcAta,
          walletAddress: wallet.publicKey.toBase58(),
          walletBalanceFloorRaw: walletBalanceFloorRaw.toString(),
          walletUsdcAta: stage.persistence.walletUsdcAta,
        };
      }
    }
    if (completedState) {
      break;
    }
  }
  if (!completedState) {
    throw new Error("Web Autodeposit setup did not reach delegation creation.");
  }
  const stageSequence = transactions.map((transaction) => transaction.stage);
  const expectedStages = [
    "initialize_subscription_authority",
    "create_policy",
    "create_recurring_delegation",
  ];
  if (JSON.stringify(stageSequence) !== JSON.stringify(expectedStages)) {
    throw new Error(
      `Unexpected Autodeposit setup stages: ${stageSequence.join(", ")}`
    );
  }

  for (const transaction of transactions) {
    await waitForFinalized(connection, transaction.signature);
  }

  const tokenAccount = await getAccount(
    connection,
    new PublicKey(completedState.walletUsdcAta),
    "finalized",
    TOKEN_PROGRAM_ID
  );
  if (
    tokenAccount.delegate?.toBase58() !==
      completedState.subscriptionAuthority ||
    tokenAccount.delegatedAmount < BigInt(1_000_000)
  ) {
    throw new Error(
      "Autodeposit setup did not install the wallet ATA delegate."
    );
  }
  for (const address of [
    completedState.policyAccount,
    completedState.recurringDelegation,
    completedState.subscriptionAuthority,
    completedState.vaultUsdcAta,
  ]) {
    if (
      !(await connection.getAccountInfo(new PublicKey(address), "finalized"))
    ) {
      throw new Error(`Autodeposit setup account ${address} is missing.`);
    }
  }
  await writeFile(args.output, JSON.stringify(completedState, null, 2));
  await writeChainTransactions({
    connection,
    output: `${args.output}.transactions.ndjson`,
    transactions,
  });
  if (args.closeReady && args.closeOutput) {
    let closeRequested = false;
    for (let attempt = 0; attempt < 1200; attempt += 1) {
      try {
        await access(args.closeReady);
        closeRequested = true;
        break;
      } catch {
        await Bun.sleep(100);
      }
    }
    if (!closeRequested) {
      throw new Error("Local verifier did not request Autodeposit close.");
    }
    const preparedClose = await vaults.prepareEarnUsdcAutodepositClose({
      cluster: LoyalCluster.MainnetBeta,
      feePayer: wallet.publicKey,
      policy: new PublicKey(completedState.policyAccount),
      policySigner: delegatedSigner.publicKey,
      recurringDelegation: new PublicKey(completedState.recurringDelegation),
      settingsPda,
      signer: wallet.publicKey,
      walletAddress: wallet.publicKey,
    });
    const closeSignature = await vaults.sdk.send(preparedClose.prepared, {
      confirm: true,
      signers: [wallet],
    });
    await waitForFinalized(connection, closeSignature);
    if (
      await connection.getAccountInfo(
        new PublicKey(completedState.recurringDelegation),
        "finalized"
      )
    ) {
      throw new Error(
        "Autodeposit close left the recurring delegation on-chain."
      );
    }
    if (
      await connection.getAccountInfo(
        new PublicKey(completedState.policyAccount),
        "finalized"
      )
    ) {
      throw new Error("Autodeposit close left the sweep policy on-chain.");
    }
    const closedTokenAccount = await getAccount(
      connection,
      new PublicKey(completedState.walletUsdcAta),
      "finalized",
      TOKEN_PROGRAM_ID
    );
    if (
      closedTokenAccount.delegate !== null ||
      closedTokenAccount.delegatedAmount !== BigInt(0)
    ) {
      throw new Error(
        "Autodeposit close left the wallet token delegate active."
      );
    }
    await writeChainTransactions({
      connection,
      output: args.closeOutput,
      transactions: [{ signature: closeSignature, stage: "close_autodeposit" }],
    });
  }
}

function issueLocalToken(state: LocalState, authSecret: string): string {
  const now = Math.floor(Date.now() / 1000);
  const claims = {
    aud: "loyal-yield-realtime",
    clientKind: "web",
    earnVaultAddress: state.vaultPubkey,
    exp: now + 300,
    iat: now,
    iss: "loyal-apps",
    scopes: ["autodeposit", "earn"],
    settingsPda: state.settingsPda,
    solanaEnv: "mainnet-beta",
    v: 1,
    walletAddress: state.walletAddress,
  };
  const payload = Buffer.from(JSON.stringify(claims)).toString("base64url");
  const signature = createHmac("sha256", authSecret)
    .update(payload)
    .digest("base64url");
  return `${payload}.${signature}`;
}

function sqlLiteral(value: string): string {
  return `'${value.replaceAll("'", "''")}'`;
}

async function runPsql(postgresUrl: string, sql: string): Promise<string> {
  const process = Bun.spawn(
    [
      "psql",
      "--no-psqlrc",
      "--quiet",
      "--tuples-only",
      "--no-align",
      "--set",
      "ON_ERROR_STOP=1",
      postgresUrl,
      "--command",
      sql,
    ],
    { stderr: "pipe", stdout: "pipe" }
  );
  const [exitCode, stderr, stdout] = await Promise.all([
    process.exited,
    new Response(process.stderr).text(),
    new Response(process.stdout).text(),
  ]);
  if (exitCode !== 0) {
    throw new Error(`psql failed: ${stderr.trim()}`);
  }
  return stdout.trim();
}

async function persistWalletBalanceFloor(args: {
  pendingFloorReady: string;
  postgresUrl: string;
  state: LocalState;
}): Promise<void> {
  for (let attempt = 0; attempt < 600; attempt += 1) {
    const updated = await runPsql(
      args.postgresUrl,
      `UPDATE loyal_yield.balance_sweep_targets
       SET wallet_balance_floor_raw = ${BigInt(
         args.state.walletBalanceFloorRaw
       )}
       WHERE settings = ${sqlLiteral(args.state.settingsPda)}
         AND wallet = ${sqlLiteral(args.state.walletAddress)}
         AND vault_index = 1
         AND policy_account = ${sqlLiteral(args.state.policyAccount)}
         AND chain_status <> 'closed'
       RETURNING id`
    );
    if (updated) {
      await writeFile(args.pendingFloorReady, "ready\n");
      return;
    }
    await Bun.sleep(100);
  }
  throw new Error("Local client did not find the pending Autodeposit target.");
}

async function listen(args: Args) {
  const expectedUiState = args.expectedUiState ?? "created";
  if (
    !(
      args.authSecret &&
      args.eventsUrl &&
      args.expectedReason &&
      args.postgresUrl &&
      args.state
    )
  ) {
    throw new Error(
      "listen requires auth, events, PostgreSQL, and state arguments."
    );
  }
  const state = JSON.parse(await readFile(args.state, "utf8")) as LocalState;
  const controller = new AbortController();
  let matched = false;
  let floorError: unknown;
  const floorUpdate =
    expectedUiState === "created"
      ? args.pendingFloorReady
        ? persistWalletBalanceFloor({
            pendingFloorReady: args.pendingFloorReady,
            postgresUrl: args.postgresUrl,
            state,
          }).catch((error) => {
            floorError = error;
            controller.abort();
          })
        : Promise.reject(
            new Error("created UI verification requires --pending-floor-ready.")
          )
      : Promise.resolve();
  const timeout = setTimeout(() => controller.abort(), 120_000);
  try {
    const response = await fetch(args.eventsUrl, {
      headers: {
        accept: "text/event-stream",
        authorization: `Bearer ${issueLocalToken(state, args.authSecret)}`,
      },
      signal: controller.signal,
    });
    if (!response.ok || !response.body) {
      throw new Error(
        `Realtime SSE connection failed with ${response.status}.`
      );
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffered = "";
    while (!matched) {
      const chunk = await reader.read();
      if (chunk.done) {
        break;
      }
      buffered += decoder.decode(chunk.value, { stream: true });
      const frames = buffered.split("\n\n");
      buffered = frames.pop() ?? "";
      for (const frame of frames) {
        const data = frame
          .split("\n")
          .filter((line) => line.startsWith("data:"))
          .map((line) => line.slice(5).trim())
          .join("\n");
        if (!data) {
          continue;
        }
        const event = JSON.parse(data) as {
          eventType?: string;
          reason?: string;
        };
        if (
          event.eventType === "earn.autodeposit.configuration.changed" &&
          event.reason === args.expectedReason
        ) {
          matched = true;
          await writeFile(
            args.output,
            JSON.stringify(
              {
                event,
                refreshPlan: { earnState: true, transactions: true },
              },
              null,
              2
            )
          );
          controller.abort();
          break;
        }
      }
    }
  } catch (error) {
    const aborted =
      typeof error === "object" &&
      error !== null &&
      "name" in error &&
      error.name === "AbortError";
    if (!(aborted && (matched || floorError))) {
      throw error;
    }
  } finally {
    clearTimeout(timeout);
  }
  if (!matched) {
    if (floorError) {
      throw floorError;
    }
    throw new Error(
      `Web SSE consumer did not receive ${args.expectedReason} Autodeposit state.`
    );
  }
  await floorUpdate;
  if (floorError) {
    throw floorError;
  }
  const current = await runPsql(
    args.postgresUrl,
    `SELECT desired_active::text || '|' || chain_status || '|' || COALESCE(wallet_balance_floor_raw, 0)::text
     FROM loyal_yield.balance_sweep_targets
     WHERE settings = ${sqlLiteral(state.settingsPda)}
       AND wallet = ${sqlLiteral(state.walletAddress)}
       AND vault_index = 1
       AND policy_account = ${sqlLiteral(state.policyAccount)}
     ORDER BY id DESC
     LIMIT 1`
  );
  const output = JSON.parse(await readFile(args.output, "utf8")) as Record<
    string,
    unknown
  >;
  if (expectedUiState === "deleted") {
    if (current && current.split("|")[1] !== "closed") {
      throw new Error(
        `Deleted Autodeposit target remained current: ${current}`
      );
    }
    await writeFile(
      args.output,
      JSON.stringify(
        {
          ...output,
          ui: {
            isOn: false,
            isPending: false,
            keepAmount: null,
            state: "deleted",
          },
        },
        null,
        2
      )
    );
    return;
  }
  if (current !== `true|active|${state.walletBalanceFloorRaw}`) {
    throw new Error(`Active Autodeposit target was not current: ${current}`);
  }
  await writeFile(
    args.output,
    JSON.stringify(
      {
        ...output,
        ui: {
          isOn: true,
          isPending: false,
          keepAmount: "2",
          state: "created",
        },
      },
      null,
      2
    )
  );
}

const { command, args } = parseArgs(process.argv.slice(2));
if (command === "setup") {
  await setup(args);
} else {
  await listen(args);
}
