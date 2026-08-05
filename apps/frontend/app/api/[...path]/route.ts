import type { NextRequest } from "next/server";

// Runtime reverse proxy for /api/* -> BACKEND_URL. Deliberately NOT a
// next.config.ts `rewrites()` entry: that config is resolved once during
// `next build` and baked into the standalone server's routing manifest, so
// a container-runtime BACKEND_URL (e.g. compose's `http://api:3000`) would
// never take effect — only a Route Handler reading `process.env` at request
// time is truly runtime-configurable in a standalone/Docker build. See
// docs/03-target-architecture.md "Deviations resolved during Phase 6".
function backendUrl(): string {
  return process.env.BACKEND_URL ?? "http://localhost:3000";
}

async function proxy(request: NextRequest, path: string[]): Promise<Response> {
  const target = `${backendUrl()}/${path.join("/")}${request.nextUrl.search}`;

  const headers = new Headers(request.headers);
  headers.delete("host");
  headers.delete("content-length");

  const hasBody = request.method !== "GET" && request.method !== "HEAD";

  const res = await fetch(target, {
    method: request.method,
    headers,
    body: hasBody ? await request.arrayBuffer() : undefined,
  });

  const responseHeaders = new Headers(res.headers);
  responseHeaders.delete("content-encoding");
  responseHeaders.delete("transfer-encoding");

  return new Response(res.body, { status: res.status, headers: responseHeaders });
}

type RouteContext = { params: Promise<{ path: string[] }> };

export async function GET(request: NextRequest, ctx: RouteContext) {
  return proxy(request, (await ctx.params).path);
}

export async function POST(request: NextRequest, ctx: RouteContext) {
  return proxy(request, (await ctx.params).path);
}

export async function PUT(request: NextRequest, ctx: RouteContext) {
  return proxy(request, (await ctx.params).path);
}

export async function PATCH(request: NextRequest, ctx: RouteContext) {
  return proxy(request, (await ctx.params).path);
}

export async function DELETE(request: NextRequest, ctx: RouteContext) {
  return proxy(request, (await ctx.params).path);
}
