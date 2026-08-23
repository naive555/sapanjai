"use client";

import { useEffect, useRef, useState } from "react";
import { CheckIcon, CopyIcon } from "lucide-react";
import { toast } from "sonner";

import { copyToClipboard } from "@/lib/clipboard";
import { cn } from "@/lib/utils";

// A UUID is 36 characters and would set the width of any column it lands in,
// so what's shown is a prefix — long enough to tell two rows apart at a
// glance, and never what gets copied. The full value stays reachable by
// hover (title) and by assistive tech (aria-label).
const VISIBLE_CHARS = 8;

/**
 * An identifier displayed compactly and copied whole.
 *
 * Worth a component rather than a `<code>`: an id that is visible but not
 * copyable is only half useful — a reader who needs one is invariably about
 * to paste it somewhere exact (an MCP endpoint URL, a support message), and
 * hand-transcribing a UUID is a coin flip. Success shows in place rather than
 * as a toast, since a table can hold a column of these and identical toasts
 * wouldn't say which row landed.
 */
export function CopyableId({
  value,
  label,
  className,
}: {
  value: string;
  /** What this identifies, for the screen-reader label — e.g. "connector ID". */
  label?: string;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  async function handleCopy() {
    if (await copyToClipboard(value)) {
      setCopied(true);
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => setCopied(false), 1600);
    } else {
      toast.error(`Couldn't copy automatically — the full ${label ?? "ID"} is ${value}`);
    }
  }

  const shortened = value.length > VISIBLE_CHARS + 4;

  return (
    <button
      type="button"
      onClick={() => void handleCopy()}
      title={value}
      aria-label={`Copy ${label ?? "ID"} ${value}`}
      className={cn(
        `group inline-flex items-center gap-1.5 rounded font-mono text-xs text-muted-foreground
        transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50
        focus-visible:outline-none`,
        className,
      )}
    >
      <span>
        {shortened ? value.slice(0, VISIBLE_CHARS) : value}
        {shortened && <span className="text-muted-foreground/50">…</span>}
      </span>
      {copied ? (
        <CheckIcon className="size-3 shrink-0 text-signal" />
      ) : (
        <CopyIcon className="size-3 shrink-0 opacity-50 transition-opacity group-hover:opacity-100" />
      )}
    </button>
  );
}
