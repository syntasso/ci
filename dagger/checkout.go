package main

import "dagger/ci/internal/dagger"

// Checkout is the shared base every component object embeds: a source
// directory plus the Name that identifies it in the release domain model
// (release.Component — see release/release.go). Name crosses as a plain
// string rather than a shared release.Component value: Dagger's codegen
// refuses to generate an exposed object (SkeOperator, Kratix, KratixCli)
// that embeds a type from outside the module's root package, even
// transitively — "cannot code-generate for foreign type Component" — so
// Component itself can't be embedded here. Each IsReleasable rebuilds
// release.Component{Name: m.Name} at that boundary instead.
//
// "Checkout" rather than "Repo": kratix and kratix-cli each have their own
// git repo, but ske-operator's source is a subdirectory of the
// enterprise-kratix monorepo, not a repo of its own. All three are still a
// checked-out source directory, which is the only thing this type needs.
//
// Go has no classical inheritance — embedding is its equivalent:
// SkeOperator/Kratix/KratixCli embed Checkout and get Source and Name
// promoted, the same way a subclass would inherit fields.
type Checkout struct {
	Name   string
	Source *dagger.Directory
}
