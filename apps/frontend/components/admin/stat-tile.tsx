/**
 * A single platform-wide count, shared by /admin (a curated subset) and
 * /admin/system (the full set on a 30s poll). Not folded into the tenant
 * side's own vocabulary — nothing there currently renders a bare number
 * tile — so this lives under components/admin rather than being mistaken
 * for something the tenant dashboard should also reach for.
 */
export function StatTile({
  label,
  value,
  hint,
}: {
  label: string;
  value: string | number;
  hint?: string;
}) {
  return (
    <div className="flex flex-col gap-1 rounded-lg border bg-card px-4 py-3">
      <span className="label-eyebrow text-muted-foreground">{label}</span>
      <span className="font-display text-2xl leading-none font-medium">{value}</span>
      {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
    </div>
  );
}
