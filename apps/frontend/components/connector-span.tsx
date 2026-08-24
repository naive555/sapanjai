"use client";

import { Fragment, useEffect, useRef, useState } from "react";

import type { AuditLogResponse, ConnectorResponse, McpKeyResponse } from "@/lib/api/endpoints";
import { cn } from "@/lib/utils";

// A key stops being trustworthy to rely on 7 days before it actually
// expires, not the instant it does — the admin needs a window to notice and
// rotate it, not a surprise outage the morning after.
const KEY_EXPIRY_WARNING_MS = 7 * 24 * 60 * 60 * 1000;

function formatTimestamp(value: string): string {
  const d = new Date(value);
  // Same UTC-date / local-time split as activity/page.tsx's row timestamps —
  // matched for consistency with the one other place a timestamp is split
  // this way, not because the mismatch is ideal.
  return `${d.toISOString().slice(0, 10)} ${d.toTimeString().slice(0, 5)}`;
}

function typeLabel(type: string): string {
  return type === "google_sheets" ? "Google Sheets" : "Generic";
}

// Standalone functions (not a `Date.now()` read at the top of the component
// body) for the same reason mcp-keys/page.tsx's deriveStatus is one: calling
// an impure function directly during render is a purity violation the
// react-compiler lint rule catches, but reading the clock inside a predicate
// passed to .filter is exactly how that file already does the equivalent
// "not expired" check.
function isUsableKey(key: McpKeyResponse): boolean {
  return !key.revokedAt && (!key.expiresAt || new Date(key.expiresAt).getTime() > Date.now());
}

function isExpiringSoon(key: McpKeyResponse): boolean {
  return !!key.expiresAt && new Date(key.expiresAt).getTime() - Date.now() < KEY_EXPIRY_WARNING_MS;
}

interface NodeSpec {
  eyebrow: string;
  value: string;
  caption: React.ReactNode;
  /** This node's own condition, independent of anything upstream of it. */
  ownBroken: boolean;
}

/**
 * One connector's full request path, drawn with the same solid/dashed
 * hairline grammar `ScopeChain` uses for the dashboard's own credential
 * chain (`components/scope-chain.tsx`) — applied here to the longer chain a
 * gateway request actually carries:
 *
 *   agent -> key -> gateway -> connector -> upstream
 *
 * Pure presentation: every prop is data the caller already fetched, so this
 * can be stacked once per connector on an overview page (Phase 5) without
 * firing N queries of its own.
 *
 * `recentLogs` should be this connector's own `mcp.*` rows, newest first
 * (docs/02-api-contract.md: "Org's logs, newest first") — the caller is
 * expected to have already filtered on `metadata.connector_id`, the same
 * filter the connector detail page's own activity table applies below this
 * component. Because the filter and the fetch both happen upstream of this
 * component, "recent" here is exactly the window the caller chose to pass —
 * there is no `since` param yet (that's Phase 4), so call/denial counts are
 * deliberately captioned "recent", never a fixed window like "24h".
 */
export function ConnectorSpan({
  connector,
  mcpKeys,
  recentLogs,
}: {
  /**
   * `null` is the explicit zero-connector mode (overview page, Phase 5): the
   * whole span renders dashed as the onboarding checklist, each node's
   * caption naming the step that creates it. Deliberately not a synthetic
   * `ConnectorResponse` stand-in — a fabricated row (a fake id, a fake
   * "generic" type) would lie to every downstream reader of this component,
   * so the connector/upstream nodes below branch on `connector === null`
   * explicitly instead of being handed a lie to render.
   */
  connector: ConnectorResponse | null;
  mcpKeys: McpKeyResponse[];
  recentLogs: AuditLogResponse[];
}) {
  // ---- agent: last seen, from the most recent session start ----
  const lastSession = recentLogs.find((log) => log.action === "mcp.session.started");

  // ---- key: usable = not revoked, not expired. Keys are minted org-wide,
  // never per-connector, so the count below is never scoped to just this
  // connector — every caption says so honestly rather than implying
  // otherwise. ----
  const usableKeys = mcpKeys.filter(isUsableKey);
  const expiringSoon = usableKeys.filter(isExpiringSoon);

  // ---- gateway: traffic through this connector, from the same recent-log
  // window "last seen" above reads from ----
  const callCount = recentLogs.filter((log) => log.action === "mcp.tool.called").length;
  const deniedCount = recentLogs.filter((log) => log.action === "mcp.tool.denied").length;

  const connectorActive = connector?.status === "active";
  const connectorError = connector?.status === "error";

  const nodes: NodeSpec[] = [
    {
      eyebrow: "agent",
      value: lastSession ? "connected" : "no agent",
      caption: lastSession ? `last seen ${formatTimestamp(lastSession.createdAt)}` : "no agent has connected yet",
      ownBroken: !lastSession,
    },
    {
      eyebrow: "key",
      value: `${usableKeys.length} key${usableKeys.length === 1 ? "" : "s"}`,
      caption:
        usableKeys.length === 0
          ? "mint a key — keys are org-wide, not per-connector"
          : `org-wide, not per-connector${
              expiringSoon.length ? ` · ${expiringSoon.length} expiring within 7 days` : ""
            }`,
      ownBroken: usableKeys.length === 0,
    },
    {
      eyebrow: "gateway",
      value: `${callCount} call${callCount === 1 ? "" : "s"}`,
      caption:
        deniedCount > 0 ? (
          <>
            <span className="text-signal">{deniedCount} denied</span>
            <span className="text-muted-foreground"> (recent)</span>
          </>
        ) : (
          "no denials (recent)"
        ),
      // The gateway itself has no independent failure mode to report here —
      // it only ever goes dashed as fallout from something to its left.
      ownBroken: false,
    },
    {
      eyebrow: "connector",
      value: connector ? connector.name : "none",
      caption: !connector
        ? "create a connector to get started"
        : connectorActive
          ? "active"
          : connectorError
            ? "check the config, then run a health check"
            : "run a health check",
      ownBroken: !connectorActive,
    },
    {
      eyebrow: "upstream",
      value: connector ? typeLabel(connector.type) : "—",
      caption: !connector
        ? "nothing to reach until a connector exists"
        : connectorActive
          ? connector.lastHealthCheckAt
            ? `checked ${formatTimestamp(connector.lastHealthCheckAt)}`
            : "checked"
          : "not reachable until the connector is active",
      // Mirrors the connector node on purpose: nothing downstream of an
      // inactive (or nonexistent) connector is actually reachable, so it
      // never reads as "connected" on its own account either.
      ownBroken: !connectorActive,
    },
  ];

  // "At most one node is emphasised: the leftmost broken one. Nothing
  // downstream of a broken link lights up" (plan §2, "Signature element —
  // the Span"). A node is dashed (unlit) if it is broken on its own account
  // or anything to its left already is — that cascade is what keeps the
  // gateway node dashed even when its own call/denial history reads fine:
  // that traffic can no longer actually reach it.
  const effectiveBroken: boolean[] = [];
  nodes.forEach((node, i) => {
    effectiveBroken.push(node.ownBroken || (i > 0 && effectiveBroken[i - 1]));
  });
  const leftmostBroken = nodes.findIndex((n) => n.ownBroken);

  // Restart the sweep only on an inactive/error -> active transition — never
  // on first paint (nothing has actually changed yet) and never on any other
  // status change, per the plan: "only when a health check flips a
  // connector to active — the one other moment a link visibly closes."
  const previousStatus = useRef<string | undefined>(undefined);
  const [sweep, setSweep] = useState(0);
  useEffect(() => {
    if (previousStatus.current !== undefined && previousStatus.current !== "active" && connectorActive) {
      setSweep((n) => n + 1);
    }
    previousStatus.current = connector?.status;
  }, [connector?.status, connectorActive]);

  return (
    <div
      key={sweep}
      className={cn(
        "flex flex-col gap-4 rounded-lg border bg-card p-4 sm:flex-row sm:items-center sm:gap-0 sm:p-5",
        sweep > 0 && "scope-sweep",
      )}
    >
      {nodes.map((node, i) => (
        <Fragment key={node.eyebrow}>
          <SpanNode
            eyebrow={node.eyebrow}
            value={node.value}
            caption={node.caption}
            broken={effectiveBroken[i]}
            emphasized={i === leftmostBroken}
            destructive={node.eyebrow === "connector" && connectorError}
          />
          {i < nodes.length - 1 && <Wire solid={!effectiveBroken[i + 1]} />}
        </Fragment>
      ))}
    </div>
  );
}

function SpanNode({
  eyebrow,
  value,
  caption,
  broken,
  emphasized,
  destructive,
}: {
  eyebrow: string;
  value: string;
  caption: React.ReactNode;
  broken: boolean;
  emphasized: boolean;
  destructive: boolean;
}) {
  return (
    <div className="flex min-w-0 flex-1 flex-col items-center gap-1.5 text-center sm:max-w-36">
      <span className="label-eyebrow">{eyebrow}</span>
      <div
        title={value}
        className={cn(
          "flex h-11 w-full items-center justify-center truncate rounded-md border px-3 font-mono text-sm",
          destructive
            ? "border-destructive/40 bg-destructive/5 text-destructive"
            : broken
              ? "border-dashed text-muted-foreground"
              : "border-wire/40 bg-wire/5 text-foreground",
          // The one visual callout in the whole span — everything else broken
          // is consequence, not new information.
          emphasized && "ring-1 ring-foreground/20",
        )}
      >
        {value}
      </div>
      <span
        className={cn(
          "text-xs leading-snug",
          emphasized ? "font-medium text-foreground" : "text-muted-foreground",
        )}
      >
        {caption}
      </span>
    </div>
  );
}

// Solid means this link resolves (--wire, the traffic colour); dashed means
// it doesn't, and the node it leads into names the fix. Same idiom as
// scope-chain.tsx's Separator, just horizontal and load-bearing over a
// longer chain.
function Wire({ solid }: { solid: boolean }) {
  return (
    <span
      aria-hidden
      className={cn(
        "hidden h-px shrink-0 self-center sm:block sm:w-8 sm:flex-1",
        solid ? "bg-wire" : "bg-transparent [border-top:1px_dashed_var(--border)]",
      )}
    />
  );
}
