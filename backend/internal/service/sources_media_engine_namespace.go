package service

import (
	"strings"
	"sync/atomic"

	"sources.local/mediaengine"
)

// sourcesMediaEngineLegacy holds the name and task id prefix under which this
// deployment already wrote task ids, billing request ids, price models, object
// keys and its task store. They come from private configuration only, so the
// public code carries no deployment-specific value. Empty means a fresh install.
type sourcesMediaEngineLegacy struct{ name, taskPrefix string }

var sourcesMediaEngineLegacyValues atomic.Pointer[sourcesMediaEngineLegacy]

// SetSourcesMediaEngineLegacy publishes the configured legacy values. The
// handler and the ledger repository check task and billing ids without access
// to the configuration, so the gateway service publishes a process snapshot at
// construction (as SetUpstreamRejectionLegacyProfile does). config.Validate has
// checked the values; "" clears them.
func SetSourcesMediaEngineLegacy(name, taskPrefix string) {
	sourcesMediaEngineLegacyValues.Store(&sourcesMediaEngineLegacy{name: name, taskPrefix: taskPrefix})
}

// sourcesMediaEngineNamespace validates again so that values published without
// config.Validate (tests, tools) still fail closed instead of writing
// unexpected values into money and storage records.
func sourcesMediaEngineNamespace() (mediaengine.Namespace, error) {
	v := sourcesMediaEngineLegacy{}
	if p := sourcesMediaEngineLegacyValues.Load(); p != nil {
		v = *p
	}
	return mediaengine.NewNamespace(v.name, v.taskPrefix)
}

// HasSourcesMediaEngineTaskPrefix reports whether id carries a task id prefix
// of the media engine, legacy or neutral. It routes task lookups and guards
// ledger batch ids; an invalid published namespace matches nothing.
func HasSourcesMediaEngineTaskPrefix(id string) bool {
	ns, err := sourcesMediaEngineNamespace()
	return err == nil && ns.HasTaskPrefix(id)
}

// The templates below are the only places these values are spelled. Each takes
// the namespace's name and substitutes it where earlier releases wrote theirs,
// so a configured legacy name reproduces the stored values byte for byte.

// sourcesMediaEngineBillingPrefix prefixes billing request ids. The id is the
// idempotency key of money commands, including for tasks spanning a release.
func sourcesMediaEngineBillingPrefix(name string) string { return name + "-image:" }

// sourcesMediaEnginePriceRoot roots the internal price models stored in usage
// records and matched against operator-configured prices.
func sourcesMediaEnginePriceRoot(name string) string { return "internal/" + name + "/" }

// sourcesMediaEngineObjectRoot roots object-storage keys of staged inputs and
// results.
func sourcesMediaEngineObjectRoot(name string) string { return "sources-" + name + "/" }

func sourcesMediaEngineBillingRequestID(ns mediaengine.Namespace, taskID string) string {
	return sourcesMediaEngineBillingPrefix(ns.Name()) + taskID
}

// isSourcesMediaEnginePriceModel accepts the legacy and the neutral root.
func isSourcesMediaEnginePriceModel(ns mediaengine.Namespace, model string) bool {
	for _, root := range ns.Forms(sourcesMediaEnginePriceRoot) {
		if strings.HasPrefix(model, root) {
			return true
		}
	}
	return false
}

// IsSourcesMediaEngineBillingRequestID reports whether requestID is the billing
// request id of task taskID in either the legacy or the neutral form. An
// invalid published name matches nothing, so the ledger refuses the command.
func IsSourcesMediaEngineBillingRequestID(requestID, taskID string) bool {
	ns, err := sourcesMediaEngineNamespace()
	if err != nil {
		return false
	}
	return ns.Matches(requestID, func(name string) string { return sourcesMediaEngineBillingPrefix(name) + taskID })
}
