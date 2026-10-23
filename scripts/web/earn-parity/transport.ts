import { type KeyMap, swapDeep, swapText } from "./translate";

export const RPC_ENDPOINT = "https://rpc.earn-parity.invalid/";

export type RecordedCall =
  | { kind: "rpc"; method: string; params: unknown; response: Record<string, unknown> }
  | { kind: "http"; method: string; url: string; body: string | null; status: number; text: string };

const READ_ONLY_HTTP = /^https:\/\/api\.kamino\.finance\//;

function isReadOnlyRpc(method: string): boolean {
  return method.startsWith("get") || method === "simulateTransaction";
}

type RpcRequest = { jsonrpc: string; id: unknown; method: string; params?: unknown };

function rpcKey(method: string, params: unknown): string {
  return `rpc ${method} ${JSON.stringify(params ?? null)}`;
}

function httpKey(method: string, url: string, body: string | null): string {
  return `http ${method} ${url} ${body ?? ""}`;
}

function requestParts(input: RequestInfo | URL, init?: RequestInit): { url: string; method: string; body: string | null } {
  const url = input instanceof Request ? input.url : String(input);
  const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
  const body = init?.body == null ? null : String(init.body);
  return { url, method, body };
}

function rpcRequests(body: string | null): RpcRequest[] {
  const parsed = JSON.parse(body ?? "null") as RpcRequest | RpcRequest[];
  return Array.isArray(parsed) ? parsed : [parsed];
}

function jsonResponse(payload: unknown): Response {
  return new Response(JSON.stringify(payload), { status: 200, headers: { "content-type": "application/json" } });
}

const IDENTITY: KeyMap = { toSource: new Map(), toTarget: new Map() };

export function recordingFetch(rpcUrl: string, realFetch: typeof fetch, log: RecordedCall[], keys: KeyMap = IDENTITY): typeof fetch {
  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    const { url, method, body } = requestParts(input, init);
    if (url === RPC_ENDPOINT) {
      const requests = rpcRequests(body);
      for (const request of requests) {
        if (!isReadOnlyRpc(request.method)) throw new Error(`earn-parity: refusing non-read RPC ${request.method}`);
      }
      const sourceBody = JSON.stringify(requests.map((request) => ({ ...request, params: swapDeep(request.params, keys.toSource) })));
      const response = await realFetch(rpcUrl, { method: "POST", headers: { "content-type": "application/json" }, body: sourceBody });
      if (!response.ok) throw new Error(`earn-parity: RPC answered HTTP ${response.status}`);
      const payload = swapDeep(await response.json(), keys.toTarget) as Record<string, unknown> | Record<string, unknown>[];
      const responses = Array.isArray(payload) ? payload : [payload];
      requests.forEach((request, index) => {
        const { id: _id, ...rest } = responses.find((r) => r.id === request.id) ?? responses[index];
        log.push({ kind: "rpc", method: request.method, params: request.params ?? null, response: rest });
      });
      return jsonResponse(Array.isArray(JSON.parse(body ?? "null")) ? responses : responses[0]);
    }
    if (!READ_ONLY_HTTP.test(url)) throw new Error(`earn-parity: refusing fetch to ${url}`);
    const response = await realFetch(url, { ...init, body: body === null ? null : swapText(body, keys.toSource) });
    const text = swapText(await response.text(), keys.toTarget);
    log.push({ kind: "http", method, url, body, status: response.status, text });
    return new Response(text, { status: response.status, headers: { "content-type": "application/json" } });
  }) as typeof fetch;
}

export function replayFetch(log: RecordedCall[]): typeof fetch {
  const queues = new Map<string, RecordedCall[]>();
  for (const call of log) {
    const key = call.kind === "rpc" ? rpcKey(call.method, call.params) : httpKey(call.method, call.url, call.body);
    queues.set(key, [...(queues.get(key) ?? []), call]);
  }
  const take = (key: string): RecordedCall => {
    const next = queues.get(key)?.shift();
    if (!next) throw new Error(`earn-parity: unrecorded call ${key.slice(0, 400)}`);
    return next;
  };
  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    const { url, method, body } = requestParts(input, init);
    if (url === RPC_ENDPOINT) {
      const requests = rpcRequests(body);
      const answers = requests.map((request) => {
        const call = take(rpcKey(request.method, request.params ?? null));
        return { ...(call.kind === "rpc" ? call.response : {}), id: request.id };
      });
      return jsonResponse(Array.isArray(JSON.parse(body ?? "null")) ? answers : answers[0]);
    }
    const call = take(httpKey(method, url, body));
    if (call.kind !== "http") throw new Error("earn-parity: fixture call kind mismatch");
    return new Response(call.text, { status: call.status, headers: { "content-type": "application/json" } });
  }) as typeof fetch;
}
