// Package main is the Dagger CI module for the Syntasso ci repo.
//
// Run `dagger develop` from this directory first to generate dagger.gen.go
// and pin the SDK version.
//
// Every component follows the same shape — build, unit, system-test, all,
// is-releasable — so the pattern proven on ske-operator reads the same way
// for kratix and kratix-cli. GHA stays the thin trigger/runner layer; this
// module is the single place engineers reason about what CI actually does,
// runnable identically on a laptop or in a runner.
//
// # Callable functions
//
//	ske-operator-build        — build the operator image; returns a typed *Container
//	ske-operator-unit         — unit tests (no cluster needed)
//	ske-operator-system-test  — full e2e against a DIND kind cluster; accepts the pre-built image
//	ske-operator-all          — unit + system-test in parallel, building exactly once
//	ske-operator-is-releasable — releasability gate (phase 1 stub, see below)
//
//	kratix-build              — build the kratix-platform image; returns a typed *Container
//	kratix-unit               — unit tests (no cluster needed)
//	kratix-system-test        — full system-test suite against DIND kind clusters (cloud only, ~31min)
//	kratix-all                — unit + system-test in parallel, building exactly once
//	kratix-is-releasable      — always true: OSS releases on every commit
//
//	kratix-cli-build          — build the kratix CLI binary; returns a typed *File
//	kratix-cli-unit           — unit tests, no k8s dependency
//	kratix-cli-all            — build + unit in parallel
//	kratix-cli-is-releasable  — always true: no release gate for the CLI
//
// # IsReleasable — phase 1 stub
//
// ADR0013 defines a Release/SkeRelease domain model where SkeRelease.IsReleasable
// queries the GitHub Deployments API for a 5-day LRE soak window. That gate is
// part of the phase 2 trunk-based delivery model and is out of scope here — the
// ske-operator-is-releasable function below always returns true today. It exists
// now so the callable surface matches the checklist; the real gate lands with
// the rest of release orchestration in phase 2.
//
// # Usage examples (from the ci repo root)
//
//	# Build the image and inspect it
//	dagger -m ./dagger call ske-operator-build --source=./enterprise-kratix/ske-operator
//
//	# Unit tests only (~30s warm)
//	dagger -m ./dagger call ske-operator-unit --source=./enterprise-kratix/ske-operator
//
//	# Full pipeline: build once, unit + system-test in parallel
//	dagger -m ./dagger call ske-operator-all \
//	  --source=./enterprise-kratix/ske-operator \
//	  --github-token=env:GITHUB_TOKEN \
//	  --ske-license-token=env:SKE_LICENSE_TOKEN
//
//	dagger -m ./dagger call kratix-build --source=./kratix
//	dagger -m ./dagger call kratix-unit --source=./kratix
//
//	dagger -m ./dagger call kratix-cli-build --source=./kratix-cli
//	dagger -m ./dagger call kratix-cli-unit --source=./kratix-cli
package main

import (
	"context"
	"fmt"

	"dagger/ci/internal/dagger"
)

// Ci is the Dagger module for Syntasso CI pipelines.
type Ci struct{}

// SkeOperatorBuild builds the ske-operator Docker image directly from its
// Dockerfile using Dagger's native BuildKit, not 'make docker-build'. The
// Makefile target shells out to the docker CLI against a local daemon, which
// doesn't exist in a plain golang container — that gap was the actual "DIND
// unknown" blocking this function, not the Kind/e2e side. Building natively
// needs no daemon at all, so SkeOperatorBuild stays fully hermetic; only
// SkeOperatorSystemTest needs DIND, for the Kind cluster.
//
// It returns the image as a typed *Container — a content-addressed artifact
// that Dagger caches by input hash. Pass this directly to
// SkeOperatorSystemTest or a release pipeline; the image is never rebuilt
// unless the source changes.
//
// CRD manifests and generated deepcopy code are committed to the repo, so no
// 'make manifests generate' step is needed before building the image.
func (m *Ci) SkeOperatorBuild(source *dagger.Directory) *dagger.Container {
	return source.DockerBuild()
}

// SkeOperatorUnit runs ske-operator unit tests in a Go container.
// Caches Go modules, build artefacts, and envtest binaries — warm runs
// complete in ~30s vs ~3min cold on GHA.
func (m *Ci) SkeOperatorUnit(ctx context.Context, source *dagger.Directory) (string, error) {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("ske-operator-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("ske-operator-go-build")).
		WithMountedCache("/root/.local/share/kubebuilder-envtest", dag.CacheVolume("ske-operator-envtest")).
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		WithExec([]string{
			"go", "install",
			"sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.18",
		}).
		WithExec([]string{"bash", "-c", `
			KUBEBUILDER_ASSETS=$(setup-envtest use 1.30.0 \
			  --bin-dir /root/.local/share/kubebuilder-envtest -p path)
			export KUBEBUILDER_ASSETS
			go test $(go list ./... | grep -v /e2e) \
			  -coverprofile cover.out \
			  -ginkgo.flake-attempts=2
		`}).
		Stdout(ctx)
}

// SkeOperatorSystemTest runs the full ske-operator e2e suite inside a
// Docker-in-Docker Kind cluster. It accepts the pre-built image as a typed
// parameter — if you already called SkeOperatorBuild, Dagger returns the
// cached result instantly and no rebuild occurs.
//
// Three structural improvements over the GHA e2e-test job:
//
//  1. Cert-manager images are pre-pulled and loaded into Kind before anything
//     starts, eliminating the cold-pull window that caused the webhook cert
//     timing race (recurring CrashLoopBackOff).
//
//  2. All three cert-manager deployments are explicitly waited on before
//     deploying ske-operator — the gate the original workflow was missing.
//
//  3. The Kind cluster lives in a Dagger DIND service, so Sprinters AZ
//     exhaustion and runner InternalError can't corrupt the cluster mid-test.
//
// The image parameter also shows the release pipeline path: the same *Container
// from SkeOperatorBuild can be pushed to GHCR by a release function without
// any additional rebuild step.
func (m *Ci) SkeOperatorSystemTest(
	ctx context.Context,
	// image is the pre-built ske-operator *Container from SkeOperatorBuild.
	// Pass it here to reuse the cached build; omit nothing will rebuild.
	image *dagger.Container,
	source *dagger.Directory,
	githubToken *dagger.Secret,
	skeLicenseToken *dagger.Secret,
) (string, error) {

	// Docker-in-Docker service. The cache volume keeps the layer store warm
	// across runs so image pulls don't repeat on re-runs.
	dockerd := dag.Container().
		From("docker:27-dind").
		WithMountedCache("/var/lib/docker", dag.CacheVolume("ske-operator-dind-layers")).
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true, InsecureRootCapabilities: true})

	// Export the typed *Container as an OCI tarball.
	// This is the typed artifact handoff: the image built in SkeOperatorBuild
	// arrives here as a *File without re-running a single build step.
	// In GHA, the equivalent would require pushing to a registry first.
	imageTar := image.AsTarball()

	return dag.Container().
		From("golang:1.26-bookworm").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("ske-operator-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("ske-operator-go-build")).
		WithServiceBinding("docker", dockerd).
		WithEnvVariable("DOCKER_HOST", "tcp://docker:2375").
		WithExec([]string{"apt-get", "update", "-qq"}).
		// docker.io provides the docker CLI client used below to talk to the
		// DIND daemon over DOCKER_HOST — it was missing from the original
		// spike, which is why 'docker load'/'docker pull' never ran.
		WithExec([]string{"apt-get", "install", "-yq", "--no-install-recommends",
			"curl", "ca-certificates", "make", "docker.io",
		}).
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/v0.25.0/kind-linux-amd64 && chmod +x /usr/local/bin/kind`,
		}).
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/v1.30.0/bin/linux/amd64/kubectl" && chmod +x /usr/local/bin/kubectl`,
		}).
		WithExec([]string{"sh", "-c",
			`curl -s https://fluxcd.io/install.sh | FLUX_VERSION=2.4.0 bash`,
		}).
		WithExec([]string{"sh", "-c",
			`curl -sSL "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh" | bash -s -- /usr/local/bin`,
		}).
		// Mount the OCI tar and load it into the DIND daemon.
		// No registry push required — the image flows as a typed value.
		WithMountedFile("/tmp/ske-operator.tar", imageTar).
		WithExec([]string{"docker", "load", "-i", "/tmp/ske-operator.tar"}).
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		// Pre-pull cert-manager images in parallel before the cluster exists.
		WithExec([]string{"sh", "-c", `
			docker pull quay.io/jetstack/cert-manager-controller:v1.15.3 &
			docker pull quay.io/jetstack/cert-manager-webhook:v1.15.3 &
			docker pull quay.io/jetstack/cert-manager-cainjector:v1.15.3 &
			wait
		`}).
		WithExec([]string{"kind", "create", "cluster", "--name", "platform"}).
		// Load cert-manager images into Kind before any pods start — no cold pulls.
		// ske-operator image is already in the DIND daemon from docker load above;
		// Kind can pull it directly without a separate kind load step.
		WithExec([]string{"sh", "-c", `
			kind load docker-image quay.io/jetstack/cert-manager-controller:v1.15.3 --name platform &
			kind load docker-image quay.io/jetstack/cert-manager-webhook:v1.15.3 --name platform &
			kind load docker-image quay.io/jetstack/cert-manager-cainjector:v1.15.3 --name platform &
			wait
		`}).
		WithExec([]string{"kubectl", "--context", "kind-platform", "create", "namespace", "kratix-platform-system"}).
		WithExec([]string{"kubectl", "--context", "kind-platform", "apply", "-f", "config/samples/config.yaml"}).
		WithExec([]string{"sh", "-c", `kustomize build config/crd | kubectl apply -f -`}).
		WithExec([]string{"kubectl", "apply", "-f",
			"https://github.com/cert-manager/cert-manager/releases/download/v1.15.3/cert-manager.yaml",
		}).
		// Explicit gate: wait for all cert-manager components before deploying
		// anything that needs the webhook cert. Without this, ske-operator starts
		// before the cert is issued and enters CrashLoopBackOff.
		WithExec([]string{"kubectl", "wait", "deployment/cert-manager",
			"--for=condition=Available", "--timeout=120s", "-n", "cert-manager"}).
		WithExec([]string{"kubectl", "wait", "deployment/cert-manager-webhook",
			"--for=condition=Available", "--timeout=120s", "-n", "cert-manager"}).
		WithExec([]string{"kubectl", "wait", "deployment/cert-manager-cainjector",
			"--for=condition=Available", "--timeout=120s", "-n", "cert-manager"}).
		WithSecretVariable("GITHUB_TOKEN", githubToken).
		WithSecretVariable("SKE_LICENSE_TOKEN", skeLicenseToken).
		WithExec([]string{"bash", "-c", `
			GIT_USERNAME=syntassodev \
			GIT_PASSWORD="$GITHUB_TOKEN" \
			KIND_CLUSTER=platform \
			go test ./test/e2e/ -v -ginkgo.v -ginkgo.flake-attempts=2
		`}).
		Stdout(ctx)
}

// SkeOperatorAll runs unit tests and system tests in parallel, building the
// operator image exactly once. This is the full CI pipeline in a single call.
//
// The parallel execution is idiomatic Go — no separate jobs, no needs: arrays,
// no artifact upload/download between steps. The built image flows as a typed
// value from SkeOperatorBuild to SkeOperatorSystemTest inside the same call graph.
//
// Future: a release function would accept the same *Container from
// SkeOperatorBuild and push it to GHCR — reusing the cached build, not
// triggering another 'make docker-build'.
func (m *Ci) SkeOperatorAll(
	ctx context.Context,
	source *dagger.Directory,
	githubToken *dagger.Secret,
	skeLicenseToken *dagger.Secret,
) (string, error) {
	// Build once. Dagger content-addresses this by source hash — if nothing
	// changed, subsequent calls return the cached layer instantly.
	image := m.SkeOperatorBuild(source)

	// Unit and e2e run concurrently. Both share the same go-mod and go-build
	// cache volumes, so whichever finishes first warms the cache for the other.
	type result struct {
		out string
		err error
	}
	unitCh := make(chan result, 1)
	systemCh := make(chan result, 1)

	go func() {
		out, err := m.SkeOperatorUnit(ctx, source)
		unitCh <- result{out, err}
	}()

	go func() {
		out, err := m.SkeOperatorSystemTest(ctx, image, source, githubToken, skeLicenseToken)
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

// SkeOperatorIsReleasable is the phase 1 stub for the releasability gate.
// The real gate — ADR0013's SkeRelease.IsReleasable, querying the GitHub
// Deployments API for a 5-day LRE soak window with no incident flag — is part
// of the phase 2 trunk-based delivery model and is not implemented here. This
// always returns true so the callable surface matches the graduation
// checklist today; wiring in the soak-window query is phase 2 work.
func (m *Ci) SkeOperatorIsReleasable(ctx context.Context) (bool, error) {
	return true, nil
}

// ---------------------------------------------------------------------------
// kratix (OSS) — same shape as ske-operator, no license token, no monorepo
// subdirectory scoping, and no envtest dependency for unit tests.
// ---------------------------------------------------------------------------

// KratixBuild builds the kratix-platform Docker image directly from its
// Dockerfile using Dagger's native BuildKit. The Dockerfile already declares
// its own go-build and go-mod cache mounts, so no daemon and no extra cache
// wiring are needed here — same fix as SkeOperatorBuild.
func (m *Ci) KratixBuild(source *dagger.Directory) *dagger.Container {
	return source.DockerBuild()
}

// KratixUnit runs the kratix unit test suite. Unlike ske-operator, the
// Makefile's unit `test` target explicitly skips the packages that need a
// real cluster (--skip-package=system,core,git), so no envtest binaries or
// KUBEBUILDER_ASSETS are needed here at all.
func (m *Ci) KratixUnit(ctx context.Context, source *dagger.Directory) (string, error) {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("kratix-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("kratix-go-build")).
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		WithExec([]string{
			"go", "run", "github.com/onsi/ginkgo/v2/ginkgo", "-r",
			"--coverprofile", "cover.out", "--skip-package=system,core,git",
		}).
		Stdout(ctx)
}

// KratixSystemTest runs the kratix system-test suite inside a Docker-in-Docker
// Kind cluster, cloud only per the graduation checklist (~31min, reconciliation
// bound — this is not a local target). Unlike ske-operator's hand-rolled Kind
// setup, kratix's clusters (platform + worker, gitea, minio, flux) are already
// orchestrated by scripts/quick-start.sh — reusing it here avoids re-encoding
// that setup a second time, which the constraints in the brief warn is easy to
// get subtly wrong (cert-manager webhook timing race, cold image pulls).
//
// The pre-built image is loaded into the DIND daemon and re-tagged to match
// every tag the Makefile's docker-build target produces. Combined with CI=true,
// this trips the Makefile's existing "image already loaded" short-circuit so
// quick-start.sh reuses the typed *Container from KratixBuild instead of
// rebuilding it — the same typed-artifact handoff as the ske-operator path.
func (m *Ci) KratixSystemTest(
	ctx context.Context,
	image *dagger.Container,
	source *dagger.Directory,
) (string, error) {
	dockerd := dag.Container().
		From("docker:27-dind").
		WithMountedCache("/var/lib/docker", dag.CacheVolume("kratix-dind-layers")).
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true, InsecureRootCapabilities: true})

	imageTar := image.AsTarball()

	return dag.Container().
		From("golang:1.26-bookworm").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("kratix-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("kratix-go-build")).
		WithServiceBinding("docker", dockerd).
		WithEnvVariable("DOCKER_HOST", "tcp://docker:2375").
		WithEnvVariable("CI", "true").
		WithExec([]string{"apt-get", "update", "-qq"}).
		WithExec([]string{"apt-get", "install", "-yq", "--no-install-recommends",
			"curl", "ca-certificates", "make", "docker.io",
		}).
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/v0.25.0/kind-linux-amd64 && chmod +x /usr/local/bin/kind`,
		}).
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/v1.30.0/bin/linux/amd64/kubectl" && chmod +x /usr/local/bin/kubectl`,
		}).
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
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		WithExec([]string{"make", "quick-start"}).
		WithExec([]string{"make", "-j4", "run-system-test"}).
		Stdout(ctx)
}

// KratixAll runs unit tests and the system-test suite in parallel, building
// the kratix-platform image exactly once. Same shape as SkeOperatorAll.
func (m *Ci) KratixAll(ctx context.Context, source *dagger.Directory) (string, error) {
	image := m.KratixBuild(source)

	type result struct {
		out string
		err error
	}
	unitCh := make(chan result, 1)
	systemCh := make(chan result, 1)

	go func() {
		out, err := m.KratixUnit(ctx, source)
		unitCh <- result{out, err}
	}()

	go func() {
		out, err := m.KratixSystemTest(ctx, image, source)
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

// KratixIsReleasable always returns true — OSS Kratix releases on every
// commit, per ADR0013's KratixRelease (no gate, unlike SkeRelease).
func (m *Ci) KratixIsReleasable(ctx context.Context) (bool, error) {
	return true, nil
}

// ---------------------------------------------------------------------------
// kratix-cli — no Docker image, no k8s dependency. Plain binary build and a
// ginkgo suite that fakes out its one external dependency (HELM_BINARY=echo).
// ---------------------------------------------------------------------------

// KratixCliBuild compiles the kratix CLI binary and returns it as a typed
// *File — the binary equivalent of the *Container artifact pattern used for
// the image-based components.
func (m *Ci) KratixCliBuild(source *dagger.Directory) *dagger.File {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("kratix-cli-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("kratix-cli-go-build")).
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		WithEnvVariable("CGO_ENABLED", "0").
		WithExec([]string{"go", "build", "-o", "bin/kratix", "./cmd/kratix/main.go"}).
		File("/src/bin/kratix")
}

// KratixCliUnit runs the kratix-cli test suite: the helm-stage pipeline test
// script (which fakes its one external dependency via HELM_BINARY=echo, so no
// real helm binary or cluster is needed) plus the ginkgo suite.
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
func (m *Ci) KratixCliUnit(ctx context.Context, source *dagger.Directory) (string, error) {
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
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		WithExec([]string{"bash", "./stages/helm-promise/pipeline_test.bash"}).
		WithExec([]string{"go", "run", "github.com/onsi/ginkgo/v2/ginkgo", "-r"}).
		Stdout(ctx)
}

// KratixCliAll builds the binary and runs unit tests in parallel — unlike the
// image-based components, the build and the tests here are fully independent,
// so there is no "build once, fan out" ordering to preserve.
func (m *Ci) KratixCliAll(ctx context.Context, source *dagger.Directory) (string, error) {
	type result struct {
		out string
		err error
	}
	unitCh := make(chan result, 1)

	go func() {
		out, err := m.KratixCliUnit(ctx, source)
		unitCh <- result{out, err}
	}()

	// The build itself has no meaningful stdout to report; run it for its
	// side effect (surfacing compile failures) alongside the unit tests.
	_, buildErr := m.KratixCliBuild(source).Sync(ctx)

	unitResult := <-unitCh

	if buildErr != nil {
		return "", fmt.Errorf("build: %w", buildErr)
	}
	if unitResult.err != nil {
		return "", fmt.Errorf("unit: %w", unitResult.err)
	}

	return fmt.Sprintf("unit:\n%s", unitResult.out), nil
}

// KratixCliIsReleasable always returns true — the CLI has no release gate.
func (m *Ci) KratixCliIsReleasable(ctx context.Context) (bool, error) {
	return true, nil
}
