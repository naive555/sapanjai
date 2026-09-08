// Package connectorhealth sweeps every organization's connectors on an
// interval and probes each one's upstream, so a dead credential is caught
// here first rather than surfacing as a customer's agent failing mid-call.
//
// Nothing here is specific to Google: the sweep calls the same
// connector.Service.CheckHealth every /connectors/:id/health-check request
// uses, so UpdateConnectorHealth and the Checker registry are reused rather
// than reimplemented, and any future adapter's Checker is picked up for
// free.
package connectorhealth

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/sapanjai/backend/internal/infra/database"
	"github.com/sapanjai/backend/internal/infra/database/db"
	"github.com/sapanjai/backend/internal/module/connector"
	"github.com/sapanjai/backend/internal/shared/apperror"
	"github.com/sapanjai/backend/internal/shared/email"
	"github.com/sapanjai/backend/internal/worker"
)

var (
	_ healthStore   = (*database.Store)(nil)
	_ healthChecker = (*connector.Service)(nil)
	_ worker.Job    = (*Job)(nil)
)

// healthStore is the subset of *database.Store this job needs, narrowed so
// unit tests can hand-mock it (same pattern as cleanupStore in
// internal/job/sessioncleanup and dispatchStore in internal/job/emaildispatch).
type healthStore interface {
	ListConnectorsForHealthCheck(ctx context.Context, batchSize int32) ([]db.ListConnectorsForHealthCheckRow, error)
	GetOrganizationOwner(ctx context.Context, organizationID uuid.UUID) (db.GetOrganizationOwnerRow, error)
	EnqueueEmail(ctx context.Context, arg db.EnqueueEmailParams) (db.EmailOutbox, error)
}

// healthChecker is the subset of *connector.Service this job depends on. It
// is what actually decrypts a connector's config, runs its registered
// Checker, and records the outcome — this job never touches encrypted_config
// or a Checker directly, so decrypted config never leaves the service that
// owns it (CLAUDE.md).
type healthChecker interface {
	CheckHealth(ctx context.Context, organizationID, connectorID uuid.UUID) (db.Connector, error)
}

// healthRenderer is the subset of *email.Renderer this job needs.
type healthRenderer interface {
	ConnectorHealth(to string, data email.ConnectorHealthData) (email.Message, error)
}

// Job sweeps a batch of connectors once per Interval.
type Job struct {
	store      healthStore
	connectors healthChecker
	render     healthRenderer
	log        *slog.Logger
	interval   time.Duration
	batchSize  int32
	appURL     string
}

// New builds the connector-health job. appURL is the frontend origin
// (config.Config.AppPublicURL) a notification's connector link is built
// against.
func New(store healthStore, connectors healthChecker, render healthRenderer, log *slog.Logger, interval time.Duration, batchSize int, appURL string) *Job {
	return &Job{
		store:      store,
		connectors: connectors,
		render:     render,
		log:        log,
		interval:   interval,
		batchSize:  int32(batchSize),
		appURL:     appURL,
	}
}

func (j *Job) Name() string            { return "connector-health" }
func (j *Job) Interval() time.Duration { return j.interval }

// Run lists up to batchSize connectors, oldest-checked-first, and probes
// each one through connector.Service.CheckHealth.
//
// Three outcomes are not failures and are not counted as such: a connector
// whose type has no registered Checker (apperror.HealthCheckUnsupported —
// e.g. the "generic" placeholder type) is skipped quietly, uncounted; and a
// connector that IS checked but found unhealthy is still "checked", not
// "failed" — CheckHealth itself returned no error, it just recorded a
// status of "error" on the row. "failed" here means CheckHealth could not
// even run the probe (the row vanished mid-sweep, the sealed config would
// not open, ...), and one connector's failure must not abort the sweep.
func (j *Job) Run(ctx context.Context) (worker.Result, error) {
	rows, err := j.store.ListConnectorsForHealthCheck(ctx, j.batchSize)
	if err != nil {
		return worker.Result{}, fmt.Errorf("list connectors for health check: %w", err)
	}

	var checked, failed, notified int

	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return worker.Result{
				Affected: int64(checked),
				Attrs:    []any{"checked", checked, "failed", failed, "notified", notified},
			}, err
		}

		wasActive := row.Status == string(connector.StatusActive)

		updated, err := j.connectors.CheckHealth(ctx, row.OrganizationID, row.ID)
		if err != nil {
			if ae, ok := err.(*apperror.Error); ok && ae.Code == apperror.HealthCheckUnsupported {
				// No registered Checker for this type. Not a failure: it is
				// never going to become healthy or unhealthy, so it must
				// not be counted or emailed about.
				continue
			}
			j.log.WarnContext(ctx, "connector health check errored",
				"connector_id", row.ID, "organization_id", row.OrganizationID, "type", row.Type, "error", err)
			failed++
			continue
		}

		checked++

		// Transition-triggered, not state-triggered: a connector that was
		// already "error" before this run stays silent, so a credential
		// broken for a week produces one email, not one per interval.
		if wasActive && updated.Status == string(connector.StatusError) {
			if j.notify(ctx, updated) {
				notified++
			}
		}
	}

	return worker.Result{
		Affected: int64(checked),
		Attrs:    []any{"checked", checked, "failed", failed, "notified", notified},
	}, nil
}

// notify enqueues a one-time alert to row's organization owner. Best-effort
// throughout, matching the repo's audit/email discipline: any failure here
// is logged and does not fail the sweep or the connector's own health-check
// outcome.
//
// The body carries only the connector's own name (row.Name — the customer's
// own label, not anything from its config) and a link built from appURL. It
// never carries the connector's config or the upstream Checker error string
// — CLAUDE.md forbids either reaching a mail body, and the upstream error
// can quote credential-adjacent detail.
func (j *Job) notify(ctx context.Context, row db.Connector) bool {
	owner, err := j.store.GetOrganizationOwner(ctx, row.OrganizationID)
	if err != nil {
		j.log.WarnContext(ctx, "connector health: could not resolve organization owner",
			"organization_id", row.OrganizationID, "connector_id", row.ID, "error", err)
		return false
	}

	displayName := ""
	if owner.DisplayName != nil {
		displayName = *owner.DisplayName
	}

	msg, err := j.render.ConnectorHealth(owner.Email, email.ConnectorHealthData{
		DisplayName:   displayName,
		ConnectorName: row.Name,
		ConnectorURL:  fmt.Sprintf("%s/connectors/%s", j.appURL, row.ID.String()),
	})
	if err != nil {
		j.log.WarnContext(ctx, "connector health: failed to render notification",
			"connector_id", row.ID, "error", err)
		return false
	}

	if _, err := j.store.EnqueueEmail(ctx, db.EnqueueEmailParams{
		ToAddress: msg.To,
		Subject:   msg.Subject,
		BodyHtml:  &msg.HTML,
		BodyText:  &msg.Text,
	}); err != nil {
		j.log.WarnContext(ctx, "connector health: failed to enqueue notification email",
			"connector_id", row.ID, "error", err)
		return false
	}

	return true
}
