import { Button } from "@/components/ui/button";

/**
 * Offset/limit pager shared by every admin list page. They all page through
 * `GET /admin/*` endpoints that return `{ items, total }` (unlike the
 * tenant-side `GET /audit-logs`'s bare array — execution plan Task 2.2), so
 * there is a real `total` to show a range against rather than a bare
 * "Previous"/"Next" pair with no sense of how much more there is.
 */
export function Pager({
  offset,
  limit,
  total,
  onOffsetChange,
}: {
  offset: number;
  limit: number;
  total: number;
  onOffsetChange: (offset: number) => void;
}) {
  const start = total === 0 ? 0 : offset + 1;
  const end = Math.min(offset + limit, total);

  return (
    <div className="flex items-center justify-between gap-3 text-sm text-muted-foreground">
      <span>
        {start}–{end} of {total}
      </span>
      <div className="flex gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={offset === 0}
          onClick={() => onOffsetChange(Math.max(0, offset - limit))}
        >
          Previous
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={offset + limit >= total}
          onClick={() => onOffsetChange(offset + limit)}
        >
          Next
        </Button>
      </div>
    </div>
  );
}
