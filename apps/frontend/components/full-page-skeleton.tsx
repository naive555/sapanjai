import { Skeleton } from "@/components/ui/skeleton";

// Shared loading placeholder for the three auth-gated render points (root
// redirect page, (auth) layout, (dashboard) layout) while session status is
// still "loading" or a client-side redirect is in flight.
export function FullPageSkeleton() {
  return (
    <div className="flex flex-1 items-center justify-center">
      <Skeleton className="h-8 w-48" />
    </div>
  );
}
