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

interface ProxyFailure {
  method: string;
  path: string;
  targetHost: string;
  code: string;
  durationMs: number;
}

// Emits exactly one line of JSON. Railway parses a single-line JSON object
// into a queryable record (`message` required, `level` optional, every other
// field filterable as `@field:value`); anything multi-line becomes one record
// per line instead, which is why an uncaught fetch failure arrives as a dozen
// interleaved fragments of Next's error dump.
//
// The fields are an explicit allowlist, never a spread of the request. This
// handler forwards the caller's Authorization header, and unlike the backend
// — where internal/shared/logger/redact.go scrubs sensitive keys centrally —
// nothing here would catch a mistake. Never log headers, bodies, or the query
// string; `path` is the pathname only, and the target is reduced to its host.
function logProxyFailure(f: ProxyFailure): void {
  console.log(
    JSON.stringify({
      level: "error",
      message: "proxy request failed",
      method: f.method,
      path: f.path,
      targetHost: f.targetHost,
      code: f.code,
      durationMs: f.durationMs,
    }),
  );
}

// undici reports the useful part on `cause` — for a refused connection that is
// an AggregateError carrying `code: "ECONNREFUSED"`, for a bad hostname an
// Error with `code: "ENOTFOUND"`. Which one it is distinguishes a wrong
// BACKEND_URL from an unset one, so it is worth pulling out.
function errorCode(err: unknown): string {
  const cause = (err as { cause?: unknown } | null)?.cause;
  const code =
    (cause as { code?: unknown } | null)?.code ?? (err as { code?: unknown } | null)?.code;
  return typeof code === "string" ? code : "UNKNOWN";
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return "invalid-backend-url";
  }
}

async function proxy(request: NextRequest, path: string[]): Promise<Response> {
  const base = backendUrl();
  const target = `${base}/${path.join("/")}${request.nextUrl.search}`;

  const headers = new Headers(request.headers);
  headers.delete("host");
  headers.delete("content-length");

  // Never forward a client-supplied X-Forwarded-For/X-Real-IP. `headers` above
  // is a copy of whatever the caller sent, and `fetch` would otherwise relay
  // it to the backend verbatim — so a request crafted with
  // `X-Forwarded-For: <anything>` reached the API completely unmodified,
  // including an address the backend's ADMIN_IP_ALLOWLIST (docs/11-admin-panel.md
  // Task 6.2) or its audit-log "ip" metadata would treat as trustworthy.
  // Deleting rather than trying to sanitize/re-derive a "real" value is
  // deliberate: this process has no reliable way to tell a genuine upstream
  // hop's entry apart from one the client fabricated once both live in the
  // same header, and guessing wrong would silently reopen exactly this hole.
  // The backend's own e.IPExtractor (apps/backend/internal/server/server.go)
  // then sees, at most, whatever this proxy's own connection looks like —
  // never anything the original caller wrote.
  headers.delete("x-forwarded-for");
  headers.delete("x-real-ip");

  const hasBody = request.method !== "GET" && request.method !== "HEAD";
  const startedAt = Date.now();

  let res: Response;
  try {
    res = await fetch(target, {
      method: request.method,
      headers,
      body: hasBody ? await request.arrayBuffer() : undefined,
    });
  } catch (err) {
    // The backend being unreachable is a gateway fault, not an application
    // bug: without this catch Next surfaces it as a 500 with a stack page,
    // which reads as "the dashboard is broken" instead of "the API is down".
    // Body shape matches the backend's httpx.ErrorResponse so lib/api/client
    // reports it through the same ApiError path as any other failure.
    logProxyFailure({
      method: request.method,
      path: request.nextUrl.pathname,
      targetHost: hostOf(base),
      code: errorCode(err),
      durationMs: Date.now() - startedAt,
    });

    return new Response(JSON.stringify({ message: "Backend unreachable" }), {
      status: 502,
      headers: { "content-type": "application/json" },
    });
  }

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
