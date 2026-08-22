"use client";

import { useEffect, useRef, useState } from "react";
import { CheckIcon, CopyIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { copyToClipboard } from "@/lib/clipboard";
import { cn } from "@/lib/utils";

/**
 * A literal the reader is meant to copy exactly — an OAuth scope, a URL, a
 * shell command — shown in the data face and selectable as a unit.
 *
 * Success is reported in place (the button flips to a check for a beat)
 * rather than as a toast: on a page carrying half a dozen of these, a stack
 * of identical "Copied to clipboard" toasts tells you nothing about *which*
 * one landed. Failure still toasts, because it needs an instruction the
 * button has no room for.
 *
 * A value containing a newline renders as a block; anything else stays on one
 * line and scrolls. Either way the text is `select-all`, so a reader whose
 * browser blocks the Clipboard API can still get it in one click-and-copy.
 */
export function CopyableCode({
  value,
  label,
  className,
}: {
  value: string;
  /** Announced to screen readers on the copy button, e.g. "read-only Sheets scope". */
  label?: string;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);
  // Held so the timer can be cleared on unmount — setCopied(false) firing
  // after the reader has navigated away is a React warning and nothing else,
  // but it's noise in a console people read while following these steps.
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  const multiline = value.includes("\n");

  async function handleCopy() {
    if (await copyToClipboard(value)) {
      setCopied(true);
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => setCopied(false), 1600);
    } else {
      toast.error("Couldn't copy automatically — select the text and copy it manually.");
    }
  }

  return (
    <div
      className={cn(
        "flex gap-2 rounded-md border bg-muted/40 py-2 pr-2 pl-3",
        multiline ? "items-start" : "items-center",
        className,
      )}
    >
      <code
        className={cn(
          "min-w-0 flex-1 font-mono text-xs leading-relaxed select-all",
          // Single-line values here are long (an authorization URL with
          // encoded scopes). truncate would hide the part a reader most
          // wants to eyeball; scrolling keeps it readable and still copies
          // whole.
          "overflow-x-auto",
          multiline ? "block whitespace-pre" : "block whitespace-nowrap",
        )}
      >
        {value}
      </code>
      <Button
        type="button"
        variant="ghost"
        size="xs"
        className="shrink-0 text-muted-foreground"
        aria-label={label ? `Copy ${label}` : "Copy"}
        onClick={() => void handleCopy()}
      >
        {copied ? <CheckIcon className="size-3.5" /> : <CopyIcon className="size-3.5" />}
        {copied ? "Copied" : "Copy"}
      </Button>
    </div>
  );
}
