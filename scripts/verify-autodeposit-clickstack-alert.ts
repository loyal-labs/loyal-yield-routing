#!/usr/bin/env bun

import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

type FailureCode =
  | "kamino_top_up_failed"
  | "yield_persistence_failed"
  | "preflight_blocked"
  | "not_actionable"
  | "fee_payer_exhausted"
  | "transaction_effect_ambiguous"
  | "idle_handoff_failed"
  | "dependency_unavailable";

type FailureCase = {
  failureCode: FailureCode;
  expectedExitCode: number;
  expectedAlertCode: string | null;
  expectedOperation: string | null;
  expectedSummary: string | null;
};

type ProbeResult = {
  alerted: boolean;
  code: string | null;
  operation: string | null;
  serviceVersion: string | null;
  summary: string | null;
};

type OtlpRequest = {
  authorization: string | null;
  body: Uint8Array;
  path: string;
};

const CASES: FailureCase[] = [
  {
    failureCode: "kamino_top_up_failed",
    expectedExitCode: 20,
    expectedAlertCode: "kamino_top_up_failed",
    expectedOperation: "top_up_autodeposit_to_kamino",
    expectedSummary: "autodeposit pull succeeded but Kamino top-up failed",
  },
  {
    failureCode: "yield_persistence_failed",
    expectedExitCode: 21,
    expectedAlertCode: "yield_persistence_failed",
    expectedOperation: "persist_autodeposit_yield_position",
    expectedSummary:
      "autodeposit top-up succeeded but yield persistence failed",
  },
  {
    failureCode: "preflight_blocked",
    expectedExitCode: 22,
    expectedAlertCode: "autodeposit_preflight_blocked",
    expectedOperation: "preflight_autodeposit_route",
    expectedSummary:
      "autodeposit route preflight blocked before any funds moved",
  },
  {
    failureCode: "not_actionable",
    expectedExitCode: 23,
    expectedAlertCode: null,
    expectedOperation: null,
    expectedSummary: null,
  },
  {
    failureCode: "fee_payer_exhausted",
    expectedExitCode: 24,
    expectedAlertCode: "autodeposit_fee_payer_exhausted",
    expectedOperation: "fund_autodeposit_fee_payer",
    expectedSummary:
      "autodeposit fee payer is out of SOL; top up the delegated signer",
  },
  {
    failureCode: "transaction_effect_ambiguous",
    expectedExitCode: 25,
    expectedAlertCode: "autodeposit_transaction_effect_ambiguous",
    expectedOperation: "reconcile_autodeposit_transaction",
    expectedSummary:
      "autodeposit transaction effect remains ambiguous after blockhash expiry",
  },
  {
    failureCode: "idle_handoff_failed",
    expectedExitCode: 26,
    expectedAlertCode: "autodeposit_idle_handoff_failed",
    expectedOperation: "publish_autodeposit_idle_vault_balance",
    expectedSummary:
      "confirmed autodeposit pull could not be published to idle-vault recovery",
  },
  {
    failureCode: "dependency_unavailable",
    expectedExitCode: 27,
    expectedAlertCode: "autodeposit_dependency_unavailable",
    expectedOperation: "retry_autodeposit_after_dependency_recovers",
    expectedSummary:
      "autodeposit dependency returned a transient server error; execution will retry",
  },
];

function assert(condition: unknown, message: string): asserts condition {
  if (!condition) {
    throw new Error(message);
  }
}

async function run(
  command: string[],
  options: { cwd: string; env?: Record<string, string | undefined> }
) {
  const process = Bun.spawn(command, {
    cwd: options.cwd,
    env: options.env ?? globalThis.process.env,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(process.stdout).text(),
    new Response(process.stderr).text(),
    process.exited,
  ]);
  return { exitCode, stdout, stderr };
}

function stripAnsi(output: string): string {
  return output.replace(/\u001b\[[0-9;]*m/g, "");
}

function resultFromOutput(output: string): ProbeResult {
  const line = output
    .split("\n")
    .find((candidate) => candidate.startsWith("VERIFIER_RESULT="));
  assert(line, `Rust probe did not emit VERIFIER_RESULT. Output:\n${output}`);
  return JSON.parse(line.slice("VERIFIER_RESULT=".length)) as ProbeResult;
}

function environmentWithout(...names: string[]): Record<string, string> {
  const environment = { ...process.env } as Record<string, string>;
  for (const name of names) {
    delete environment[name];
  }
  return environment;
}

function startOtlpServer(requests: OtlpRequest[]) {
  for (let attempt = 0; attempt < 25; attempt += 1) {
    const port = 40_000 + Math.floor(Math.random() * 20_000);
    try {
      return Bun.serve({
        hostname: "127.0.0.1",
        port,
        async fetch(request: Request) {
          requests.push({
            authorization: request.headers.get("authorization"),
            body: new Uint8Array(await request.arrayBuffer()),
            path: new URL(request.url).pathname,
          });
          return new Response(new Uint8Array(), {
            headers: { "content-type": "application/x-protobuf" },
            status: 200,
          });
        },
      });
    } catch {
      // Try another loopback port without touching any external service.
    }
  }
  throw new Error("Unable to allocate an isolated loopback OTLP port");
}

async function main() {
  const scriptDirectory = dirname(fileURLToPath(import.meta.url));
  const repositoryRoot = resolve(scriptDirectory, "..");
  const executorModule = join(
    repositoryRoot,
    "scripts/execute-autodeposit-policy.ts"
  );
  const [workflow, lightDockerfile, laserstreamDockerfile, renderBlueprint] =
    await Promise.all(
      [
        ".github/workflows/rust-image-build.yml",
        "Dockerfile.light-workers",
        "Dockerfile.laserstream-workers",
        "render.yaml",
      ].map((path) => readFile(join(repositoryRoot, path), "utf8"))
    );
  assert(
    workflow.includes("LOYAL_IMAGE_VERSION=sha-${{ github.sha }}"),
    "worker-images workflow does not pass the immutable image version"
  );
  for (const [name, dockerfile] of [
    ["light-workers", lightDockerfile],
    ["laserstream-workers", laserstreamDockerfile],
  ]) {
    assert(
      dockerfile.includes("ARG LOYAL_IMAGE_VERSION") &&
        dockerfile.includes("ENV LOYAL_IMAGE_VERSION=${LOYAL_IMAGE_VERSION}"),
      `${name} does not embed LOYAL_IMAGE_VERSION in its runtime image`
    );
  }
  assert(
    !renderBlueprint.includes("OBSERVABILITY_SERVICE_VERSION"),
    "render.yaml still duplicates the image version per service"
  );
  const isolatedRoot = await mkdtemp(
    join(tmpdir(), "loyal-autodeposit-alert-verifier-")
  );
  const otlpRequests: OtlpRequest[] = [];
  const otlpServer = startOtlpServer(otlpRequests);

  try {
    const typescriptProbe = join(isolatedRoot, "executor-exit-probe.ts");
    await writeFile(
      typescriptProbe,
      [
        `import { autodepositExecutorFailureExitCode } from ${JSON.stringify(
          pathToFileURL(executorModule).href
        )};`,
        "const failureCode = process.argv[2] as",
        `  ${CASES.map((failureCase) => `| ${JSON.stringify(failureCase.failureCode)}`).join("\n  ")};`,
        "process.exit(autodepositExecutorFailureExitCode(failureCode));",
        "",
      ].join("\n")
    );

    const cargoEnvironment = {
      ...process.env,
      CARGO_TARGET_DIR: join(isolatedRoot, "cargo-target"),
    };
    const build = await run(
      [
        "cargo",
        "build",
        "--offline",
        "--locked",
        "--quiet",
        "--manifest-path",
        join(repositoryRoot, "Cargo.toml"),
        "-p",
        "balance-sweep-autodeposit-trigger",
        "--bin",
        "autodeposit-alert-contract-probe",
      ],
      { cwd: repositoryRoot, env: cargoEnvironment }
    );
    assert(
      build.exitCode === 0,
      `Rust probe failed to build.\nstdout:\n${build.stdout}\nstderr:\n${build.stderr}`
    );
    const rustProbe = join(
      cargoEnvironment.CARGO_TARGET_DIR,
      "debug/autodeposit-alert-contract-probe"
    );

    const verified: Array<{
      alerted: boolean;
      failureCode: string;
      exitCode: number;
    }> = [];
    for (const failureCase of CASES) {
      const executor = await run(
        [
          "sh",
          "-c",
          'exec bun "$1" "$2"',
          "autodeposit-alert-verifier",
          typescriptProbe,
          failureCase.failureCode,
        ],
        {
          cwd: repositoryRoot,
          env: {
            ...process.env,
            AUTODEPOSIT_KAMINO_TOP_UP_FAILED_EXIT_CODE: "20",
            AUTODEPOSIT_YIELD_PERSISTENCE_FAILED_EXIT_CODE: "21",
            AUTODEPOSIT_PREFLIGHT_BLOCKED_EXIT_CODE: "22",
            AUTODEPOSIT_NOT_ACTIONABLE_EXIT_CODE: "23",
            AUTODEPOSIT_FEE_PAYER_EXHAUSTED_EXIT_CODE: "24",
            AUTODEPOSIT_TRANSACTION_EFFECT_AMBIGUOUS_EXIT_CODE: "25",
            AUTODEPOSIT_IDLE_HANDOFF_FAILED_EXIT_CODE: "26",
            AUTODEPOSIT_DEPENDENCY_UNAVAILABLE_EXIT_CODE: "27",
          },
        }
      );
      assert(
        executor.exitCode === failureCase.expectedExitCode,
        `${failureCase.failureCode} crossed the shell boundary as ${executor.exitCode}; expected ${failureCase.expectedExitCode}`
      );

      const requestCountBeforeProbe = otlpRequests.length;
      const rust = await run([rustProbe, String(executor.exitCode)], {
        cwd: repositoryRoot,
        env: {
          ...environmentWithout("OBSERVABILITY_SERVICE_VERSION"),
          LOYAL_IMAGE_VERSION: "sha-verifier-image",
          OBSERVABILITY_ENABLED: "true",
          OBSERVABILITY_ENVIRONMENT: "verification",
          OBSERVABILITY_INGESTION_API_KEY: "verification-only",
          OBSERVABILITY_OTLP_ENDPOINT: `http://127.0.0.1:${otlpServer.port}`,
          RENDER_GIT_COMMIT: "sha-verifier-render",
          RUST_LOG: "error",
        },
      });
      assert(
        rust.exitCode === 0,
        `Rust probe failed for ${failureCase.failureCode}.\nstdout:\n${rust.stdout}\nstderr:\n${rust.stderr}`
      );
      const combinedOutput = `${rust.stdout}\n${rust.stderr}`;
      const plainOutput = stripAnsi(combinedOutput);
      const result = resultFromOutput(plainOutput);
      assert(
        result.alerted === (failureCase.expectedAlertCode !== null),
        "Rust alert suppression mismatch"
      );
      assert(
        result.code === failureCase.expectedAlertCode,
        "Rust error code mismatch"
      );
      assert(
        result.operation === failureCase.expectedOperation,
        "Rust operation mismatch"
      );
      assert(
        result.summary === failureCase.expectedSummary,
        "Rust summary mismatch"
      );
      assert(
        result.serviceVersion === "sha-verifier-image",
        "Embedded image version did not take precedence over RENDER_GIT_COMMIT"
      );
      const logRequest = otlpRequests
        .slice(requestCountBeforeProbe)
        .find((request) => request.path === "/v1/logs");
      if (failureCase.expectedAlertCode === null) {
        assert(
          !logRequest,
          "Non-actionable executor exit unexpectedly reached OTLP logs"
        );
      } else {
        assert(
          plainOutput.includes(
            `error_code="${failureCase.expectedAlertCode}"`
          ),
          `OperationalError omitted error_code for ${failureCase.failureCode}`
        );
        assert(
          plainOutput.includes(
            `loyal.error.code="${failureCase.expectedAlertCode}"`
          ),
          `OperationalError omitted loyal.error.code for ${failureCase.failureCode}`
        );
        assert(
          logRequest,
          `OTLP exporter did not send /v1/logs for ${failureCase.failureCode}`
        );
        assert(
          logRequest.authorization === "verification-only",
          "OTLP exporter did not send the configured authorization header"
        );
        const protobufText = new TextDecoder().decode(logRequest.body);
        for (const expected of [
          "error_code",
          "loyal.error.code",
          failureCase.failureCode,
          failureCase.expectedOperation!,
          "sha-verifier-image",
        ]) {
          assert(
            protobufText.includes(expected),
            `OTLP protobuf payload omitted ${expected}`
          );
        }
      }
      verified.push({
        alerted: result.alerted,
        failureCode: failureCase.failureCode,
        exitCode: executor.exitCode,
      });
    }

    const generic = await run([rustProbe, "1"], {
      cwd: repositoryRoot,
      env: {
        ...process.env,
        OBSERVABILITY_ENABLED: "false",
        RUST_LOG: "error",
      },
    });
    assert(generic.exitCode === 0, "Generic Rust fallback probe failed");
    assert(
      resultFromOutput(`${generic.stdout}\n${generic.stderr}`).code ===
        "autodeposit_executor_failed",
      "Unknown executor failures must retain the generic alert code"
    );

    const override = await run([rustProbe, "20"], {
      cwd: repositoryRoot,
      env: {
        ...process.env,
        LOYAL_IMAGE_VERSION: "sha-verifier-image",
        OBSERVABILITY_ENABLED: "false",
        OBSERVABILITY_SERVICE_VERSION: "sha-verifier-override",
        RENDER_GIT_COMMIT: "sha-verifier-render",
        RUST_LOG: "error",
      },
    });
    assert(override.exitCode === 0, "Service-version override probe failed");
    assert(
      resultFromOutput(`${override.stdout}\n${override.stderr}`)
        .serviceVersion === "sha-verifier-override",
      "OBSERVABILITY_SERVICE_VERSION must override the embedded image version"
    );

    const renderFallback = await run([rustProbe, "20"], {
      cwd: repositoryRoot,
      env: {
        ...environmentWithout(
          "LOYAL_IMAGE_VERSION",
          "OBSERVABILITY_SERVICE_VERSION"
        ),
        OBSERVABILITY_ENABLED: "false",
        RENDER_GIT_COMMIT: "sha-verifier-render",
        RUST_LOG: "error",
      },
    });
    assert(renderFallback.exitCode === 0, "Render fallback probe failed");
    assert(
      resultFromOutput(`${renderFallback.stdout}\n${renderFallback.stderr}`)
        .serviceVersion === "sha-verifier-render",
      "RENDER_GIT_COMMIT must remain the final service-version fallback"
    );

    console.log(
      JSON.stringify(
        {
          status: "pass",
          isolation: "temporary local processes only",
          verified,
          genericFallback: "autodeposit_executor_failed",
          clickstackAttributes: ["error_code", "loyal.error.code"],
          otlpTransport: `http://127.0.0.1:${otlpServer.port}/v1/logs`,
          serviceVersionPrecedence: [
            "OBSERVABILITY_SERVICE_VERSION",
            "LOYAL_IMAGE_VERSION",
            "RENDER_GIT_COMMIT",
          ],
        },
        null,
        2
      )
    );
  } finally {
    otlpServer.stop(true);
    await rm(isolatedRoot, { recursive: true, force: true });
  }
}

main().catch((error) => {
  console.error(error instanceof Error ? error.stack : String(error));
  process.exitCode = 1;
});
