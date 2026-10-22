import { afterEach, expect, spyOn, test } from "bun:test";
import { NextRequest } from "next/server";

import { GET } from "./route";

afterEach(() => {
  spyOn(globalThis, "fetch").mockRestore();
});

async function status(target: string): Promise<number> {
  const req = new NextRequest(
    `http://localhost/api/telegram/proxy-image?url=${encodeURIComponent(target)}`
  );
  return (await GET(req)).status;
}

test("proxy-image accepts only exact allowed hosts or dot-boundary subdomains", async () => {
  const fetchSpy = spyOn(globalThis, "fetch").mockImplementation(
    async () => new Response("img", { headers: { "content-type": "image/png" } })
  );

  expect(await status("https://evilt.me/a.png")).toBe(400);
  expect(await status("https://xcdn1.telesco.pe/a.png")).toBe(400);
  expect(fetchSpy).not.toHaveBeenCalled();

  expect(await status("https://t.me/a.png")).toBe(200);
  expect(await status("https://cdn1.telesco.pe/a.png")).toBe(200);
  expect(await status("https://x.cdn1.telesco.pe/a.png")).toBe(200);

  fetchSpy.mockImplementation(
    async () => new Response(null, { status: 302, headers: { location: "https://evil.example/a.png" } })
  );
  expect(await status("https://t.me/redir.png")).not.toBe(200);
});

test("proxy-image refuses upstream responses that are not images", async () => {
  spyOn(globalThis, "fetch").mockImplementation(
    async () => new Response("<html></html>", { headers: { "content-type": "text/html" } })
  );
  expect(await status("https://t.me/some/page")).toBe(415);

  spyOn(globalThis, "fetch").mockRestore();
  spyOn(globalThis, "fetch").mockImplementation(
    async () => new Response("<svg/>", { headers: { "content-type": "image/svg+xml" } })
  );
  expect(await status("https://t.me/a.svg")).toBe(415);
});
