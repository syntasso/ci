// Package release holds the ADR0013 releasability domain model: Component,
// Artifact, Release and its per-component variants, plus Commit/Tag. These
// types are never returned from an exported Dagger function (SkeOperator,
// Kratix, and KratixCli return plain bool/string/error), so they don't need
// to live in the module's root package — only Dagger objects do.
package release

import (
	"context"
	"time"
)

// Component is a releasable unit tracked by this module — the name behind
// each object in this package (SkeOperator, Kratix, KratixCli today; beyond
// phase 1, backstage-plugin and headlamp per ADR0013).
type Component struct {
	Name string
}

// Commit identifies the source revision a Release was cut from.
type Commit struct {
	SHA  string
	Repo string
}

// Tag marks a Commit as a release candidate under a version name — e.g. the
// phase 2 trunk-based model's `releasable/YYYY-MM-DD/SHORT_SHA` tags cut by
// the daily cron once a Deployment clears its soak window. Not part of
// ADR0013's original domain model; added here so the commit -> tag -> release
// path is a real type, not just a version string. Phase 1 doesn't yet plumb a
// commit SHA into these Dagger calls, so Release.Tag stays its zero value
// until that wiring lands in phase 2.
type Tag struct {
	Name   string
	Commit Commit
}

// Artifact is a versioned publishable output of a Release.
type Artifact struct {
	Type    string // "image", "chart", "binary", "sbom"
	Version string
	Ref     string // GHCR ref or S3 path
}

// Release is the base releasability type. IsReleasable defaults to true —
// releasable on every commit, no gate. KratixRelease relies on this default
// unmodified; SkeRelease overrides it.
type Release struct {
	Component Component
	Tag       Tag
	Artifacts []Artifact
	Type      string // discriminator: "ske", "kratix", "kratix-cli"
}

// IsReleasable is the no-gate default.
func (r Release) IsReleasable(ctx context.Context) (bool, error) {
	return true, nil
}

// SkeRelease gates on the phase 2 trunk-based delivery model: a Deployment
// for ReleaseSHA must be older than SoakWindow with no incident flag, per
// ADR0013. Overrides Release.IsReleasable rather than inheriting it, so the
// real GitHub Deployments API query can land in phase 2 without changing
// SkeOperator's call site.
type SkeRelease struct {
	Release
	SoakWindow time.Duration // default 5 days
	ReleaseSHA string
}

// IsReleasable is a phase 1 stub. The real gate — query the GitHub
// Deployments API for ReleaseSHA, require time.Since(deployment.CreatedAt) >
// SoakWindow and no incident flag — is phase 2 trunk-based delivery work, out
// of scope here. Always true today.
func (r SkeRelease) IsReleasable(ctx context.Context) (bool, error) {
	return true, nil
}

// KratixRelease is OSS Kratix: releases on every commit. It doesn't override
// IsReleasable — Release's default (always true) is exactly right.
type KratixRelease struct {
	Release
}
