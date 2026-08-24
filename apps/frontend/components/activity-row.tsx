import { PermissionToken } from "@/components/permission-token";
import { cn } from "@/lib/utils";

type Metadata = Record<string, unknown>;

// Metadata arrives as Record<string, unknown> on the wire — these guards are
// the only thing standing between a malformed/absent field and a thrown
// error, so every renderer below reads through them rather than indexing
// metadata directly.
function str(metadata: Metadata, key: string): string | undefined {
  const value = metadata[key];
  return typeof value === "string" ? value : undefined;
}

function num(metadata: Metadata, key: string): number | undefined {
  const value = metadata[key];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function strArray(metadata: Metadata, key: string): string[] | undefined {
  const value = metadata[key];
  if (!Array.isArray(value) || value.length === 0) return undefined;
  return value.every((entry) => typeof entry === "string") ? (value as string[]) : undefined;
}

// A call's identifying context (which spreadsheet, which file) — present on
// some tools and not others, and secondary to what happened and what it
// cost, so it stays muted and trails the primary fields.
const SECONDARY_KEYS = ["spreadsheet_id", "sheet_name", "file_id", "folder_id", "filter_columns"] as const;

function SecondaryFields({ metadata }: { metadata: Metadata }) {
  const parts: string[] = [];
  for (const key of SECONDARY_KEYS) {
    if (key === "filter_columns") {
      const columns = strArray(metadata, key);
      if (columns) parts.push(columns.join(","));
      continue;
    }
    const value = str(metadata, key);
    if (value) parts.push(value);
  }
  if (!parts.length) return null;
  return <span className="ml-2 text-muted-foreground">{parts.join(" · ")}</span>;
}

function ToolCalled({ metadata }: { metadata: Metadata }) {
  const tool = str(metadata, "tool") ?? "unknown tool";
  const duration = num(metadata, "duration_ms");
  const rows = num(metadata, "row_count");
  // >2s is the line between "normal roundtrip" and "this is doing real
  // work" — worth marking as traffic (--wire), not as a problem.
  const heavy = duration !== undefined && duration > 2000;

  return (
    <span>
      <span className="font-mono text-xs text-foreground">{tool}</span>
      {duration !== undefined && (
        <span className={cn("ml-2 font-mono text-xs", heavy ? "font-medium text-wire" : "text-muted-foreground")}>
          {duration}ms
        </span>
      )}
      {rows !== undefined && <span className="ml-1 text-xs text-muted-foreground">· {rows} rows</span>}
      <SecondaryFields metadata={metadata} />
    </span>
  );
}

function ToolDenied({ metadata }: { metadata: Metadata }) {
  const tool = str(metadata, "tool") ?? "unknown tool";
  const missingPermission = str(metadata, "missing_permission");

  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <span className="font-mono text-xs text-foreground">{tool}</span>
      {missingPermission ? (
        <PermissionToken action={missingPermission} />
      ) : (
        <span className="text-xs text-muted-foreground">—</span>
      )}
    </span>
  );
}

function RateLimitHit({ metadata }: { metadata: Metadata }) {
  const tool = str(metadata, "tool") ?? "unknown tool";
  return <span className="font-mono text-xs text-destructive">{tool} rate limited</span>;
}

function FileDownloaded({ metadata }: { metadata: Metadata }) {
  const fileId = str(metadata, "file_id") ?? "—";
  const mimeType = str(metadata, "mime_type");
  return (
    <span className="font-mono text-xs text-foreground">
      {fileId}
      {mimeType && <span className="ml-2 text-muted-foreground">{mimeType}</span>}
    </span>
  );
}

function SessionStarted({ metadata }: { metadata: Metadata }) {
  const connectorId = str(metadata, "connector_id") ?? "—";
  // Fires on every agent reconnect, so it must not compete visually with
  // rows that need attention — quiet mono, nothing else on the line.
  return <span className="font-mono text-xs text-muted-foreground/70">{connectorId}</span>;
}

function ConnectorEvent({ action, metadata }: { action: string; metadata: Metadata }) {
  if (action === "connector.deleted") {
    // connector.deleted is the one connector.* action without a name/type —
    // the row only carries the id of what's gone.
    return <span className="font-mono text-xs text-muted-foreground">{str(metadata, "connector_id") ?? "—"}</span>;
  }

  const name = str(metadata, "name") ?? "—";
  const type = str(metadata, "type");
  const rotated = action === "connector.updated" && metadata["config_rotated"] === "true";

  return (
    <span className="font-mono text-xs text-foreground">
      {name}
      {type && <span className="ml-2 text-muted-foreground">{type}</span>}
      {rotated && <span className="ml-2 text-signal">config rotated</span>}
    </span>
  );
}

// Anything not covered above — an action this page doesn't know about yet,
// or a shape that doesn't match what's expected — falls back to the raw
// key=value dump the whole column used to be, so a future adapter's new
// action degrades gracefully instead of rendering blank.
function FallbackFields({ metadata }: { metadata: Metadata }) {
  const entries = Object.entries(metadata ?? {});
  if (!entries.length) return <span className="text-muted-foreground">—</span>;
  return (
    <>
      {entries.map(([key, value]) => (
        <span key={key} className="mr-3 font-mono text-xs text-muted-foreground">
          <span className="text-muted-foreground/60">{key}=</span>
          {String(value)}
        </span>
      ))}
    </>
  );
}

/**
 * Renders one audit row's `metadata` as the shape it actually has, keyed on
 * `action`. Shapes are verified against internal/module/mcp/service.go and
 * internal/module/connector/service.go — see the table in the phase plan.
 * `metadata` is Record<string, unknown> on the wire, so every field is read
 * through the typeof-guards above rather than assumed: a missing or
 * malformed field degrades to "—" or omission, it never throws.
 */
export function ActivityDetail({ action, metadata }: { action: string; metadata: Metadata | null }) {
  // Callers are expected to pass `log.metadata ?? {}`, but the wire type is
  // nullable and this component is shared across pages — absorbing null here
  // means a caller that forgets renders "—" instead of throwing.
  if (metadata === null || typeof metadata !== "object") {
    return <span className="text-muted-foreground">—</span>;
  }

  switch (action) {
    case "mcp.tool.called":
      return <ToolCalled metadata={metadata} />;
    case "mcp.tool.denied":
      return <ToolDenied metadata={metadata} />;
    case "mcp.ratelimit.hit":
      return <RateLimitHit metadata={metadata} />;
    case "mcp.file.downloaded":
      return <FileDownloaded metadata={metadata} />;
    case "mcp.session.started":
      return <SessionStarted metadata={metadata} />;
    case "connector.created":
    case "connector.updated":
    case "connector.deleted":
      return <ConnectorEvent action={action} metadata={metadata} />;
    default:
      return <FallbackFields metadata={metadata} />;
  }
}
