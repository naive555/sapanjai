import { Badge } from "@/components/ui/badge";

/**
 * A connector's health, in the palette's own vocabulary.
 *
 * "Active" carries --wire rather than the neutral `secondary` grey it used
 * to: a reachable connector *is* the working link, and --wire is the token
 * globals.css reserves for the connection itself (it is the same colour the
 * Span draws its solid hairlines in, so a lit wire and a live connector read
 * as the same fact in two places). Grey said nothing about the one state
 * this product exists to produce.
 *
 * Deliberately not extended to the MCP-key table's own "Active" badge. A
 * valid credential is not traffic, and spending --wire on both would blur
 * the distinction the Span depends on — the key node and the connector node
 * are separate links in the chain precisely because they fail independently.
 */
export function ConnectorStatus({ status }: { status: string }) {
  if (status === "active") {
    return (
      <Badge variant="outline" className="border-wire/40 bg-wire/10 text-wire">
        Active
      </Badge>
    );
  }
  if (status === "error") return <Badge variant="destructive">Error</Badge>;
  return <Badge variant="outline">Inactive</Badge>;
}
