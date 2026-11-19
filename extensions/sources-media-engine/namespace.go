package mediaengine

import (
	"errors"
	"regexp"
	"strings"
)

// NeutralName names the engine in every persisted or cross-process value of a
// deployment that has no data from before the engine had a neutral name.
const NeutralName = "media-engine"

// NeutralSelector is the engine value in the operator's image-engine registry.
const NeutralSelector = "media"

// NeutralTaskPrefix starts every task id of a fresh installation.
const NeutralTaskPrefix = "mediatask"

var (
	legacyNamePattern       = regexp.MustCompile(`^[a-z0-9]{2,32}$`)
	legacyTaskPrefixPattern = regexp.MustCompile(`^[a-z0-9]{2,16}$`)
	// taskIDShape is the namespace-independent form of a task id. It bounds
	// operator-supplied ids before the namespace decides which are this store's.
	taskIDShape = regexp.MustCompile(`^[a-z0-9]{2,16}_[a-f0-9]{32}$`)
)

// Namespace is the one place that decides which name appears in persisted and
// cross-process values. A deployment that already holds data written under an
// earlier name supplies it as the legacy name, so every value it writes stays
// byte-for-byte what it wrote before; the public code never carries that name.
// The zero value is the neutral namespace of a fresh installation.
type Namespace struct{ legacy, legacyTaskPrefix string }

// NewNamespace validates the legacy name and legacy task id prefix (both ""
// select the neutral namespace). They describe one existing deployment, so
// exactly one of them is a misconfiguration. Errors never echo a value because
// the values are private configuration.
func NewNamespace(legacyName, legacyTaskPrefix string) (Namespace, error) {
	if (legacyName == "") != (legacyTaskPrefix == "") {
		return Namespace{}, errors.New("media engine legacy name and legacy task prefix must be set together")
	}
	// "sub2api" is the registry's other engine value and cannot name this one.
	if legacyName != "" && (!legacyNamePattern.MatchString(legacyName) || legacyName == "sub2api") {
		return Namespace{}, errors.New("invalid media engine legacy name")
	}
	if legacyTaskPrefix != "" && !legacyTaskPrefixPattern.MatchString(legacyTaskPrefix) {
		return Namespace{}, errors.New("invalid media engine legacy task prefix")
	}
	return Namespace{legacy: legacyName, legacyTaskPrefix: legacyTaskPrefix}, nil
}

// Legacy reports the configured legacy name, or "" for the neutral namespace.
func (n Namespace) Legacy() string { return n.legacy }

// Name is substituted wherever earlier code wrote the engine's name.
func (n Namespace) Name() string {
	if n.legacy != "" {
		return n.legacy
	}
	return NeutralName
}

// Selector is the canonical engine value of registry entries.
func (n Namespace) Selector() string {
	if n.legacy != "" {
		return n.legacy
	}
	return NeutralSelector
}

// IsSelector accepts both the neutral and the configured legacy engine value,
// so an operator can rewrite the registry at any time without a flag day.
func (n Namespace) IsSelector(v string) bool {
	return v == NeutralSelector || (n.legacy != "" && v == n.legacy)
}

// Forms returns the values a template takes under this namespace: the one to
// write first, then the neutral one when it differs. Readers accept every form.
func (n Namespace) Forms(template func(name string) string) []string {
	if n.legacy == "" {
		return []string{template(NeutralName)}
	}
	return []string{template(n.legacy), template(NeutralName)}
}

// Matches reports whether v is one of the forms of template.
func (n Namespace) Matches(v string, template func(name string) string) bool {
	for _, form := range n.Forms(template) {
		if v == form {
			return true
		}
	}
	return false
}

// TaskPrefix starts new task ids. Task ids are stored, used as ledger batch ids
// and returned to API clients, so a legacy deployment keeps its prefix.
func (n Namespace) TaskPrefix() string {
	if n.legacyTaskPrefix != "" {
		return n.legacyTaskPrefix
	}
	return NeutralTaskPrefix
}

func (n Namespace) taskPrefixes() []string {
	if n.legacyTaskPrefix == "" {
		return []string{NeutralTaskPrefix}
	}
	return []string{n.legacyTaskPrefix, NeutralTaskPrefix}
}

// newTaskID builds a task id from 32 lowercase hex characters.
func (n Namespace) newTaskID(hex string) string { return n.TaskPrefix() + "_" + hex }

// IsTaskID reports whether id is a complete task id of this namespace, with the
// legacy or the neutral prefix.
func (n Namespace) IsTaskID(id string) bool {
	return taskIDShape.MatchString(id) && n.HasTaskPrefix(id)
}

// HasTaskPrefix reports whether id starts with one of this namespace's task id
// prefixes. Callers that route by prefix leave full validation to the store.
func (n Namespace) HasTaskPrefix(id string) bool {
	for _, prefix := range n.taskPrefixes() {
		if strings.HasPrefix(id, prefix+"_") {
			return true
		}
	}
	return false
}

func keyCheckValue(name string) string { return "sources-" + name + "-v1" }

// keyCheck is the plaintext sealed into store_metadata when a store is created.
func (n Namespace) keyCheck() string { return keyCheckValue(n.Name()) }
