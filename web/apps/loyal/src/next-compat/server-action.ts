import { currentRouter, navigate } from "./navigation";

type Reply = { value?: unknown; redirect?: string };

export async function callAction(url: string, args: unknown[], headers: Record<string, string>): Promise<unknown> {
  const forms = args.filter((arg): arg is FormData => arg instanceof FormData);
  if (forms.length > 1) throw new Error(`${url}: at most one FormData argument`);
  let body: BodyInit;
  const sent = { ...headers };
  if (forms.length) {
    const form = new FormData();
    form.set("$args", JSON.stringify(args.map((arg) => (arg instanceof FormData ? { $formData: true } : arg))));
    for (const [key, value] of forms[0].entries()) form.append(key, value);
    body = form;
  } else {
    body = JSON.stringify(args);
    sent["Content-Type"] = "application/json";
  }
  const response = await fetch(url, { method: "POST", headers: sent, body, credentials: "same-origin" });
  if (response.status === 401) {
    window.location.assign(`/login?next=${encodeURIComponent(window.location.pathname + window.location.search)}`);
    return new Promise(() => {});
  }
  if (!response.ok) throw new Error(`${url}: ${response.status}`);
  const reply = (await response.json()) as Reply;
  const router = currentRouter();
  if (reply.redirect) {
    if (router) navigate(router, reply.redirect, false);
    else window.location.assign(reply.redirect);
    return undefined;
  }
  if (router) await router.invalidate();
  return reply.value;
}
