import { Table, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cn } from "@/lib/utils";

export type Column = string | { label: string; align?: "end"; className?: string };

/**
 * The tables are the product — five of the six pages are one. Keeping their
 * shell in a single component is what stops the column rhythm, rule weight,
 * and header treatment from drifting page to page.
 */
export function DataTable({ columns, children }: { columns: Column[]; children: React.ReactNode }) {
  return (
    <div className="overflow-hidden rounded-lg border bg-card">
      {/* Cell rhythm is set once here rather than per-cell on every page. */}
      <Table className="[&_td]:px-3 [&_td]:py-2.5">
        <TableHeader>
          <TableRow className="border-b bg-muted/40 hover:bg-muted/40">
            {columns.map((column, index) => {
              const config = typeof column === "string" ? { label: column } : column;
              return (
                <TableHead
                  key={config.label || index}
                  className={cn(
                    "label-eyebrow h-9 px-3 font-normal",
                    config.align === "end" && "text-right",
                    config.className,
                  )}
                >
                  {config.label}
                </TableHead>
              );
            })}
          </TableRow>
        </TableHeader>
        {children}
      </Table>
    </div>
  );
}
