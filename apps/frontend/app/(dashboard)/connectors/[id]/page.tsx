import { ConnectorDetailClient } from "./page-client";

export default async function ConnectorDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;

  // GATEWAY_URL is where the wiring snippet below tells an MCP client to
  // point at — the Sapanjai *API* host, which is not this dashboard's own
  // host in any real deployment (that's the entire reason
  // app/api/[...path]/route.ts's reverse proxy exists). Deliberately not a
  // `NEXT_PUBLIC_`-prefixed var: Next.js inlines those at `next build`,
  // baking the value into the image, which contradicts this repo's
  // deliberate runtime-config stance (see the long comment on BACKEND_URL in
  // app/api/[...path]/route.ts, and .env.local.example). A plain server-only
  // var read here, in a server component, at request time, is what actually
  // stays configurable per-deployment without a rebuild. When it's unset,
  // the client component below falls back to the browser's own origin —
  // which can only be resolved in the browser, not here.
  const gatewayUrl = process.env.GATEWAY_URL ?? null;

  return <ConnectorDetailClient connectorId={id} gatewayUrl={gatewayUrl} />;
}
