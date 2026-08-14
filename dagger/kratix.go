package main

import (
	"context"
	"fmt"

	"dagger/ci/internal/dagger"
	"dagger/ci/release"
)

// Kratix is the OSS kratix component, scoped to a source checkout. Same
// shape as SkeOperator, no license token, no monorepo subdirectory scoping,
// and no envtest dependency for unit tests.
type Kratix struct {
	Checkout
}

// Build builds the kratix-platform Docker image directly from its Dockerfile
// using Dagger's native BuildKit. The Dockerfile already declares its own
// go-build and go-mod cache mounts, so no daemon and no extra cache wiring
// are needed here — same fix as SkeOperator.Build.
func (m *Kratix) Build() *dagger.Container {
	return m.Source.DockerBuild()
}

// Unit runs the kratix unit test suite. Unlike ske-operator, the Makefile's
// unit `test` target explicitly skips the packages that need a real cluster
// (--skip-package=system,core,git), so no envtest binaries or
// KUBEBUILDER_ASSETS are needed here at all.
func (m *Kratix) Unit(ctx context.Context) (string, error) {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("kratix-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("kratix-go-build")).
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src").
		WithExec([]string{
			"go", "run", "github.com/onsi/ginkgo/v2/ginkgo", "-r",
			"--coverprofile", "cover.out", "--skip-package=system,core,git",
		}).
		Stdout(ctx)
}

// Platform brings up a running kratix platform on a Kind cluster inside a
// dedicated Docker-in-Docker service, using the pre-built kratix image (the
// same typed-artifact handoff as everywhere else — no rebuild, no registry
// round-trip). Extracted out of SystemTest so it's independently reusable:
// per the phase-1 survey, ske-cortex-controller, ske-portal-controller, and
// k8s-health-agent all need "kratix already running" as a prerequisite for
// their own e2e suites, and today each reimplements that bring-up itself.
// None of those three are wrapped in Dagger yet — this is the shared piece
// ready for when they are, dogfooded immediately below by SystemTest itself.
//
// Returns the DIND service — bind it via WithServiceBinding("docker", ...)
// and set DOCKER_HOST=tcp://docker:2375 to reach the same cluster with
// kratix already installed on it, same as SystemTest does.
//
// Unlike ske-operator's hand-rolled Kind setup, kratix's clusters (platform
// + worker, gitea, minio, flux) are already orchestrated by
// scripts/quick-start.sh — reusing it here avoids re-encoding that setup a
// second time, which the constraints in the brief warn is easy to get
// subtly wrong (cert-manager webhook timing race, cold image pulls).
//
// Not validated end-to-end: this is a refactor of SystemTest's own
// bring-up logic, and SystemTest itself was already an unvalidated,
// cloud-only function before this change (~31min, not a local target). This
// carries the same disclosed limitation forward, now on top of a fresh
// extraction — needs a real CI run to prove, same as before.
func (m *Kratix) Platform(ctx context.Context, image *dagger.Container) (*dagger.Service, error) {
	dockerd := dag.Container().
		From("docker:27-dind").
		WithMountedCache("/var/lib/docker", dag.CacheVolume("kratix-dind-layers")).
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true, InsecureRootCapabilities: true})

	imageTar := image.AsTarball()

	_, err := kindToolchain(dag.Container().
		From("golang:1.26-bookworm").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("kratix-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("kratix-go-build")).
		WithServiceBinding("docker", dockerd).
		WithEnvVariable("DOCKER_HOST", "tcp://docker:2375").
		WithEnvVariable("CI", "true")).
		WithMountedFile("/tmp/kratix.tar", imageTar).
		// Re-tag the pre-built image to match every tag docker-build produces,
		// so the Makefile's own "already loaded" check (CI=true + docker image
		// inspect) finds it under all of them, not just one.
		WithExec([]string{"sh", "-c", `
			docker load -i /tmp/kratix.tar
			IMAGE_ID=$(docker images -q | head -n1)
			docker tag "$IMAGE_ID" docker.io/syntasso/kratix-platform:dev
			docker tag "$IMAGE_ID" docker.io/syntasso/kratix-platform:latest
			docker tag "$IMAGE_ID" docker.io/syntasso/kratix-platform-quickstart:latest
			docker tag "$IMAGE_ID" syntassodev/kratix-platform:dev
		`}).
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src").
		WithExec([]string{"make", "quick-start"}).
		Sync(ctx)
	if err != nil {
		return nil, fmt.Errorf("kratix platform bring-up: %w", err)
	}

	return dockerd, nil
}

// SystemTest runs the kratix system-test suite against the platform Platform
// brings up, cloud only per the graduation checklist (~31min,
// reconciliation bound — this is not a local target).
func (m *Kratix) SystemTest(ctx context.Context, image *dagger.Container) (string, error) {
	dockerd, err := m.Platform(ctx, image)
	if err != nil {
		return "", fmt.Errorf("platform: %w", err)
	}

	ctr := dag.Container().
		From("golang:1.26-bookworm").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("kratix-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("kratix-go-build")).
		WithServiceBinding("docker", dockerd).
		WithEnvVariable("DOCKER_HOST", "tcp://docker:2375")

	return kindToolchain(ctr).
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src").
		WithExec([]string{"make", "-j4", "run-system-test"}).
		Stdout(ctx)
}

// All runs unit tests and the system-test suite in parallel, building the
// kratix-platform image exactly once. Same shape as SkeOperator.All.
func (m *Kratix) All(ctx context.Context) (string, error) {
	image := m.Build()

	type result struct {
		out string
		err error
	}
	unitCh := make(chan result, 1)
	systemCh := make(chan result, 1)

	go func() {
		out, err := m.Unit(ctx)
		unitCh <- result{out, err}
	}()

	go func() {
		out, err := m.SystemTest(ctx, image)
		systemCh <- result{out, err}
	}()

	unitResult := <-unitCh
	systemResult := <-systemCh

	if unitResult.err != nil {
		return "", fmt.Errorf("unit: %w", unitResult.err)
	}
	if systemResult.err != nil {
		return "", fmt.Errorf("system-test: %w", systemResult.err)
	}

	return fmt.Sprintf("unit:\n%s\n\nsystem-test:\n%s", unitResult.out, systemResult.out), nil
}

// IsReleasable dispatches through KratixRelease — OSS Kratix releases on
// every commit, per ADR0013's domain model (no gate, unlike SkeRelease).
func (m *Kratix) IsReleasable(ctx context.Context) (bool, error) {
	rel := release.KratixRelease{
		Release: release.Release{Component: release.Component{Name: m.Name}, Type: "kratix"},
	}
	return rel.IsReleasable(ctx)
}
