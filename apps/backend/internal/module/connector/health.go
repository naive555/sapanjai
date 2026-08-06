package connector

import "context"

// Checker probes one connector type's upstream. Implementations land with
// their adapters (docs/05-mcp-gateway.md, Phase 2); the skeleton registers
// none, so CheckHealth currently returns HEALTH_CHECK_UNSUPPORTED for every
// type.
type Checker interface {
	// Type is the connector type this checker handles.
	Type() Type

	// Check probes the upstream using the connector's decrypted config. A
	// nil error means healthy.
	//
	// Two contracts implementations must honour: config is the caller's
	// only copy of a customer credential — do not retain it, log it, or
	// send it anywhere but the upstream; and any returned error is logged
	// by the service, so it must not embed credential material (no raw
	// request URLs with keys in them, no echoed request bodies).
	Check(ctx context.Context, config map[string]any) error
}

// Registry maps a connector type to the Checker that probes it.
type Registry map[Type]Checker

// NewRegistry builds a Registry keyed by each checker's Type(). Called with
// no arguments today — see server.New.
func NewRegistry(checkers ...Checker) Registry {
	r := make(Registry, len(checkers))
	for _, c := range checkers {
		r[c.Type()] = c
	}
	return r
}
