"use client";

import { useState } from "react";
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";

import { Pager } from "@/components/admin/pager";
import { Badge } from "@/components/ui/badge";
import { CopyableId } from "@/components/copyable-id";
import { DataTable } from "@/components/data-table";
import { PageHeader, TableMessage } from "@/components/page-header";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { ApiError } from "@/lib/api/client";
import { listAdminUsers } from "@/lib/api/endpoints";

const PAGE_SIZE = 50;
const ALL_ROLES = "__all__";
const ALL_BANNED = "__all__";

function formatDate(value: string | null): string {
  return value ? new Date(value).toISOString().slice(0, 10) : "—";
}

export default function AdminUsersPage() {
  const [search, setSearch] = useState("");
  const [role, setRole] = useState(ALL_ROLES);
  const [banned, setBanned] = useState(ALL_BANNED);
  const [offset, setOffset] = useState(0);

  const filters = {
    search: search || undefined,
    role: role === ALL_ROLES ? undefined : (role as "superadmin" | "support" | "none"),
    banned: banned === ALL_BANNED ? undefined : banned === "true",
    limit: PAGE_SIZE,
    offset,
  };

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["admin", "users", filters],
    queryFn: () => listAdminUsers(filters),
  });

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="users" description="Every account on the platform, searchable by email or name.">
        <Input
          aria-label="Search users"
          placeholder="Search email or name…"
          className="w-56"
          value={search}
          onChange={(e) => {
            setSearch(e.target.value);
            setOffset(0);
          }}
        />
        <Select
          value={role}
          onValueChange={(value) => {
            setRole(value ?? ALL_ROLES);
            setOffset(0);
          }}
        >
          <SelectTrigger className="w-36">
            <SelectValue>{(v: string) => (v === ALL_ROLES ? "All roles" : v)}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL_ROLES}>All roles</SelectItem>
            <SelectItem value="superadmin">Superadmin</SelectItem>
            <SelectItem value="support">Support</SelectItem>
            <SelectItem value="none">Not staff</SelectItem>
          </SelectContent>
        </Select>
        <Select
          value={banned}
          onValueChange={(value) => {
            setBanned(value ?? ALL_BANNED);
            setOffset(0);
          }}
        >
          <SelectTrigger className="w-32">
            <SelectValue>{(v: string) => (v === ALL_BANNED ? "All" : v === "true" ? "Banned" : "Active")}</SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL_BANNED}>All</SelectItem>
            <SelectItem value="true">Banned</SelectItem>
            <SelectItem value="false">Active</SelectItem>
          </SelectContent>
        </Select>
      </PageHeader>

      <DataTable columns={["User", "Platform role", "Status", "Orgs", "Created"]}>
        <TableBody>
          {isLoading ? (
            <TableMessage colSpan={5}>Loading…</TableMessage>
          ) : isError ? (
            <TableMessage colSpan={5}>
              {error instanceof ApiError ? error.message : "Failed to load users."}
            </TableMessage>
          ) : data?.items.length ? (
            data.items.map((user) => (
              <TableRow key={user.id}>
                <TableCell>
                  <Link
                    href={`/admin/users/${user.id}`}
                    className="font-mono text-[0.8125rem] underline-offset-4 hover:underline"
                  >
                    {user.email}
                  </Link>
                  {user.displayName && <span className="ml-2 text-xs text-muted-foreground">{user.displayName}</span>}
                  <CopyableId value={user.id} label="user ID" className="mt-0.5 block" />
                </TableCell>
                <TableCell className="text-sm">
                  {user.platformRole ? (
                    <Badge variant="outline" className="uppercase">
                      {user.platformRole}
                    </Badge>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </TableCell>
                <TableCell className="flex flex-wrap gap-1.5">
                  {user.bannedAt && <Badge variant="destructive">Banned</Badge>}
                  {user.isVerified ? (
                    <Badge variant="outline" className="border-wire/40 bg-wire/10 text-wire">
                      Verified
                    </Badge>
                  ) : (
                    <Badge variant="outline">Unverified</Badge>
                  )}
                </TableCell>
                <TableCell className="text-sm text-muted-foreground">{user.orgCount}</TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">{formatDate(user.createdAt)}</TableCell>
              </TableRow>
            ))
          ) : (
            <TableMessage colSpan={5}>No users match these filters.</TableMessage>
          )}
        </TableBody>
      </DataTable>

      {data && data.total > 0 && (
        <Pager offset={offset} limit={PAGE_SIZE} total={data.total} onOffsetChange={setOffset} />
      )}
    </div>
  );
}
