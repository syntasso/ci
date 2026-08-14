package main

import (
	"context"
	"fmt"

	"dagger/ci/internal/dagger"
	"dagger/ci/release"
)

// KratixCli is the kratix-cli component, scoped to a source checkout. No
// Docker image, no k8s dependency — a plain binary build and a ginkgo suite
// that fakes out its one external dependency (HELM_BINARY=echo).
type KratixCli struct {
	Repo
}

// Build compiles the kratix CLI binary and returns it as a typed *File — the
// binary equivalent of the *Container artifact pattern used for the
// image-based components.
func (m *KratixCli) Build() *dagger.File {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("kratix-cli-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("kratix-cli-go-build")).
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src").
		WithEnvVariable("CGO_ENABLED", "0").
		WithExec([]string{"go", "build", "-o", "bin/kratix", "./cmd/kratix/main.go"}).
		File("/src/bin/kratix")
}

// Unit runs the kratix-cli test suite: the helm-stage pipeline test script
// (which fakes its one external dependency via HELM_BINARY=echo, so no real
// helm binary or cluster is needed) plus the ginkgo suite.
//
// The helm-stage script shells out to yq (mikefarah/yq, not present in the
// base golang image — running it locally against a host with yq installed
// masked this). The full `ginkgo -r` run also exercises
// internal/terraform_module_test.go, which calls the real `terraform` binary
// against a private fixture module repo — that's why the GHA workflow sets up
// an SSH deploy key before `make test`. terraform is installed below so the
// binary exists, but this function does not yet accept the SSH key needed to
// reach the private fixture repo, so the terraform-module package will fail
// here until that's threaded through. Known gap, not yet resolved.
func (m *KratixCli) Unit(ctx context.Context) (string, error) {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("kratix-cli-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("kratix-cli-go-build")).
		WithExec([]string{"sh", "-c", "apt-get update -qq && apt-get install -yq --no-install-recommends unzip"}).
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/yq https://github.com/mikefarah/yq/releases/download/v4.53.3/yq_linux_amd64 && chmod +x /usr/local/bin/yq`,
		}).
		WithExec([]string{"sh", "-c", `
			curl -sSLo /tmp/terraform.zip https://releases.hashicorp.com/terraform/1.9.8/terraform_1.9.8_linux_amd64.zip
			cd /usr/local/bin && unzip -o /tmp/terraform.zip terraform
		`}).
		WithMountedDirectory("/src", m.Source).
		WithWorkdir("/src").
		WithExec([]string{"bash", "./stages/helm-promise/pipeline_test.bash"}).
		WithExec([]string{"go", "run", "github.com/onsi/ginkgo/v2/ginkgo", "-r"}).
		Stdout(ctx)
}

// All builds the binary and runs unit tests in parallel — unlike the
// image-based components, the build and the tests here are fully
// independent, so there is no "build once, fan out" ordering to preserve.
func (m *KratixCli) All(ctx context.Context) (string, error) {
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

// IsReleasable dispatches through the base Release type — ADR0013 doesn't
// define a dedicated KratixCliRelease, and the CLI has no release gate, so
// Release's default (always true) stands in directly.
func (m *KratixCli) IsReleasable(ctx context.Context) (bool, error) {
	rel := release.Release{Component: m.component(), Type: "kratix-cli"}
	return rel.IsReleasable(ctx)
}
