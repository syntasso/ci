// Package main is the Dagger CI module for the Syntasso ci repo.
//
// Run `dagger develop` from this directory first to generate dagger.gen.go
// and pin the SDK version.
//
// Each constructor below returns a typed component object — SkeOperator,
// Kratix, KratixCli — scoped to a source directory, chainable from the CLI:
//
//	dagger call ske-operator --source=./enterprise-kratix/ske-operator build
//	dagger call ske-operator --source=./enterprise-kratix/ske-operator unit
//	dagger call ske-operator --source=./enterprise-kratix/ske-operator all \
//	  --github-token=env:GITHUB_TOKEN --ske-license-token=env:SKE_LICENSE_TOKEN
//
//	dagger call kratix --source=./kratix build
//	dagger call kratix --source=./kratix unit
//
//	dagger call kratix-cli --source=./kratix-cli build
//	dagger call kratix-cli --source=./kratix-cli unit
//
// All three satisfy Pipeline (pipeline.go): Unit and IsReleasable have the
// same shape everywhere, compiler-enforced. Build and SystemTest stay
// concrete per component. Domain types — Component, Artifact, Release,
// SkeRelease, KratixRelease, Commit, Tag — live in release.go, per ADR0013;
// each component's IsReleasable dispatches through its Release value rather
// than returning a bare bool.
//
// GHA stays the thin trigger/runner layer; this module is the single place
// engineers reason about what CI actually does, runnable identically on a
// laptop or in a runner.
//
// # IsReleasable — phase 1 stub
//
// SkeRelease.IsReleasable is meant to query the GitHub Deployments API for a
// 5-day LRE soak window (ADR0013's phase 2 trunk-based delivery model). That
// query isn't implemented yet — see release.go — so it always returns true
// today. It exists now so the callable surface matches the graduation
// checklist; the real gate lands with the rest of release orchestration in
// phase 2.
package main

import "dagger/ci/internal/dagger"

// Ci is the Dagger module for Syntasso CI pipelines.
type Ci struct{}

// SkeOperator scopes subsequent calls to the ske-operator source directory.
func (m *Ci) SkeOperator(source *dagger.Directory) *SkeOperator {
	return &SkeOperator{Source: source}
}

// Kratix scopes subsequent calls to the kratix source directory.
func (m *Ci) Kratix(source *dagger.Directory) *Kratix {
	return &Kratix{Source: source}
}

// KratixCli scopes subsequent calls to the kratix-cli source directory.
func (m *Ci) KratixCli(source *dagger.Directory) *KratixCli {
	return &KratixCli{Source: source}
}
