package main

import (
	"dagger/ci/internal/dagger"
	"dagger/ci/release"
)

// Repo is the shared base every component object embeds: a Dagger object is
// always scoped to a source checkout and identified by a Component name.
// Go has no classical inheritance — this is its idiomatic equivalent:
// SkeOperator/Kratix/KratixCli embed Repo and get Source and component()
// promoted, the same way a subclass would inherit fields and methods.
type Repo struct {
	Source *dagger.Directory
	Name   string
}

// component identifies this Repo in the release domain model.
func (r Repo) component() release.Component {
	return release.Component{Name: r.Name}
}
