import { cn } from "@/lib/utils";

/**
 * An inline note attached to the thing it qualifies, in the two weights this
 * product actually needs.
 *
 * `note` is the dashed-muted idiom the connector forms already use — context
 * the reader would otherwise have to infer, not a warning.
 *
 * `boundary` is reserved for a security boundary a reader will otherwise
 * assume works some other way, and is the one surface outside RBAC that
 * spends `--signal`. Per globals.css that colour means "elevated privilege",
 * and a connector's spreadsheet/folder allowlist is exactly that: the thing
 * standing between an agent and every file the OAuth account can reach.
 * Keeping it rare is what keeps it readable, so `boundary` is not a general
 * "important" style — if two of them end up on one screen, one of them is
 * really a `note`.
 */
export function Callout({
  variant = "note",
  title,
  children,
  className,
}: {
  variant?: "note" | "boundary";
  title?: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "rounded-md px-3 py-2.5 text-sm",
        variant === "note" && "border border-dashed bg-muted/40 text-muted-foreground",
        variant === "boundary" &&
          "border border-signal/30 bg-signal-muted/40 text-foreground dark:bg-signal-muted/25",
        className,
      )}
    >
      {title && (
        <div
          className={cn(
            "mb-1 font-medium",
            variant === "boundary" ? "text-signal" : "text-foreground",
          )}
        >
          {title}
        </div>
      )}
      <div className={cn(variant === "boundary" && "text-muted-foreground")}>{children}</div>
    </div>
  );
}
