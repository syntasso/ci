package main

import (
	"context"
	"fmt"

	"dagger/ci/internal/dagger"
	"dagger/ci/release"
)

// BackstageController is the backstage-controller component: a Go
// controller living in the enterprise-kratix monorepo, in the
// backstage-controller subdirectory — the same monorepo-subdirectory
// situation as SkeOperator, a different subdirectory of the same checkout.
type BackstageController struct {
	Checkout
}

// Build builds the backstage-controller image directly from its Dockerfile
// using Dagger's native BuildKit — same fix as SkeOperator.Build and
// Kratix.Build, no daemon needed. The Dockerfile builds two binaries
// (backstage-controller and backstage-generator) into one image; that's a
// Dockerfile implementation detail, invisible here.
func (m *BackstageController) Build() *dagger.Container {
	return m.Source.DockerBuild()
}

// Unit runs the backstage-controller unit test suite under envtest, same
// pattern as SkeOperator.Unit. Its Makefile computes ENVTEST_K8S_VERSION and
// ENVTEST_VERSION dynamically from go.mod (`go list -m ... k8s.io/api` /
// `... controller-runtime`) rather than hardcoding them the way ske-operator
// does; this function hardcodes the values they resolve to today (1.36 and
// release-0.24) — same tradeoff every dagger call makes versus a Makefile
// that recomputes them on every run.
func (m *BackstageController) Unit(ctx context.Context) (string, error) {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("backstage-controller-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("backstage-controller-go-build")).
		WithMountedCache("/root/.local/share/kubebuilder-envtest", dag.CacheVolume("backstage-controller-envtest")).
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src").
		WithExec([]string{
			"go", "install",
			"sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.24",
		}).
		WithExec([]string{"bash", "-c", `
			KUBEBUILDER_ASSETS=$(setup-envtest use 1.36 \
			  --bin-dir /root/.local/share/kubebuilder-envtest -p path)
			export KUBEBUILDER_ASSETS
			go test $(go list ./... | grep -v /e2e | grep -v /ske-backstage-generator/system) \
			  -coverprofile cover.out
		`}).
		Stdout(ctx)
}

// All builds and runs unit tests in parallel — same shape as
// KratixCli.All. No SystemTest exists for this component yet: its e2e suite
// (the Makefile's test-e2e) needs a running kratix installation as a
// prerequisite, not just a bare Kind cluster like SkeOperator.SystemTest —
// that dependency-on-another-component wiring is real, unfinished work, not
// implemented here. Known gap, not yet resolved.
func (m *BackstageController) All(ctx context.Context) (string, error) {
	type result struct {
		out string
		err error
	}
	unitCh := make(chan result, 1)

	go func() {
		out, err := m.Unit(ctx)
		unitCh <- result{out, err}
	}()

	// The build itself has no meaningful stdout to report; run it for its
	// side effect (surfacing compile failures) alongside the unit tests.
	_, buildErr := m.Build().Sync(ctx)

	unitResult := <-unitCh

	if buildErr != nil {
		return "", fmt.Errorf("build: %w", buildErr)
	}
	if unitResult.err != nil {
		return "", fmt.Errorf("unit: %w", unitResult.err)
	}

	return fmt.Sprintf("unit:\n%s", unitResult.out), nil
}

// IsReleasable dispatches through the base Release type — ADR0013 defines
// BackstagePluginRelease for the separate npm-packaged plugin (a different
// component, different repo), but no dedicated type for this one, so
// Release's default (always true) stands in directly, same treatment as
// KratixCli.
func (m *BackstageController) IsReleasable(ctx context.Context) (bool, error) {
	rel := release.Release{Component: release.Component{Name: m.Name}, Type: "backstage-controller"}
	return rel.IsReleasable(ctx)
}
