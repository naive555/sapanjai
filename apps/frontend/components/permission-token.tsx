import { cn } from "@/lib/utils";

/**
 * Renders a permission string as the grammar it actually is, so its breadth is
 * legible at a glance rather than requiring the reader to parse the text.
 *
 * Precedence, per the backend's RequirePermission middleware:
 *   `*`  >  `resource:verb`  >  `resource:*`
 *
 * Wildcards are the dangerous part of a grant, so they carry --signal; the
 * literal segments stay ink. A reader scanning a role's permission list sees
 * violet exactly where authority is broad.
 */
export function PermissionToken({ action, className }: { action: string; className?: string }) {
  const base = cn(
    "inline-flex items-center rounded-sm border px-1.5 py-0.5 font-mono text-xs leading-none",
    className,
  );

  if (action === "*") {
    return (
      <span
        className={cn(base, "border-signal/40 bg-signal/10 px-2 text-signal")}
        title="Grants every action"
      >
        <span aria-hidden>*</span>
        <span className="sr-only">all actions</span>
      </span>
    );
  }

  const separator = action.indexOf(":");
  if (separator === -1) {
    return <span className={cn(base, "border-border bg-muted/60 text-foreground")}>{action}</span>;
  }

  const resource = action.slice(0, separator);
  const verb = action.slice(separator + 1);
  const isWildcard = verb === "*";

  return (
    <span
      className={cn(
        base,
        isWildcard ? "border-signal/30 bg-signal/[0.07]" : "border-border bg-muted/60",
      )}
      title={isWildcard ? `Grants every action on ${resource}` : undefined}
    >
      <span className="text-foreground">{resource}</span>
      <span className="text-muted-foreground/70">:</span>
      <span className={isWildcard ? "text-signal" : "text-foreground"}>{verb}</span>
    </span>
  );
}
