package model

import "regexp"

// Portable validation uses these immutable patterns repeatedly across the
// complete snapshot. Compile once while preserving the existing exact rules.
var (
	stateProjectPattern    = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)
	stateIdentifierPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`)
	stateBranchPattern     = regexp.MustCompile(`^aih/[a-zA-Z0-9_-]+$`)
	stateRevisionPattern   = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
	stateHashPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	stateRetryKeyPattern   = regexp.MustCompile(`^(pre-implementation|review)/[a-z][a-z0-9_-]{0,63}$`)
	stateRolePattern       = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	stateArtifactPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}\.(png|jpg|jpeg|txt|json)$`)
	stateReasonPattern     = regexp.MustCompile(`^[a-z_]+$`)
	statePlanKeyPattern    = regexp.MustCompile(PlanKeyPattern)
)
