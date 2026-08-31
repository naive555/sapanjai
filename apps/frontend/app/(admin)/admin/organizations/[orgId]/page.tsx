import { AdminOrganizationDetailClient } from "./page-client";

export default async function AdminOrganizationDetailPage({ params }: { params: Promise<{ orgId: string }> }) {
  const { orgId } = await params;
  return <AdminOrganizationDetailClient orgId={orgId} />;
}
