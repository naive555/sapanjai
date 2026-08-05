import { cn } from "@/lib/utils";

// Authority is encoded twice — by color and by the marker — so the tier
// survives greyscale and color blindness. The marker is drawn in CSS rather
// than set as a glyph (◐ and ○ render near-identically in most monos, which
// collapsed admin and member into the same shape). Three unambiguous states:
// filled disc, hollow ring, nothing at all. "member" carries no elevated
// authority, so it gets no marker.
const TIERS: Record<string, { marker: "disc" | "ring" | "none"; className: string }> = {
  owner: {
    marker: "disc",
    className: "border-signal/35 bg-signal/10 text-signal",
  },
  admin: {
    marker: "ring",
    className: "border-wire/40 bg-wire/10 text-wire",
  },
  member: {
    marker: "none",
    className: "border-border bg-transparent text-muted-foreground",
  },
};

export function RoleBadge({ role, className }: { role: string; className?: string }) {
  const tier = TIERS[role] ?? TIERS.member;

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-sm border px-1.5 py-0.5",
        "font-mono text-[0.6875rem] leading-none tracking-[0.08em] uppercase",
        tier.className,
        className,
      )}
    >
      {tier.marker !== "none" && (
        <span
          aria-hidden
          className={cn(
            "size-1.5 shrink-0 rounded-full",
            tier.marker === "disc" ? "bg-current" : "border border-current",
          )}
        />
      )}
      {role}
    </span>
  );
}
