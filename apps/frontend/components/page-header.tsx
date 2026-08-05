export function PageHeader({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children?: React.ReactNode;
}) {
  return (
    <header className="flex flex-wrap items-end justify-between gap-x-6 gap-y-3 border-b pb-5">
      <div className="min-w-0 space-y-1.5">
        <h1 className="font-display text-2xl leading-none font-medium sm:text-[1.75rem]">{title}</h1>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      {children && <div className="flex shrink-0 flex-wrap items-center gap-2">{children}</div>}
    </header>
  );
}

/**
 * Table empty and loading states. An empty screen is an invitation to act, so
 * the copy names the next step instead of just reporting absence.
 */
export function TableMessage({ colSpan, children }: { colSpan: number; children: React.ReactNode }) {
  return (
    <tr>
      <td colSpan={colSpan} className="px-3 py-14 text-center text-sm text-muted-foreground">
        {children}
      </td>
    </tr>
  );
}

/** Shared shell for the data tables — a panel on paper, hairline-ruled. */
export function Panel({ children, className }: { children: React.ReactNode; className?: string }) {
  return (
    <div className={`overflow-hidden rounded-lg border bg-card ${className ?? ""}`}>{children}</div>
  );
}
