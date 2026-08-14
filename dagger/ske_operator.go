package main

import (
	"context"
	"fmt"
	"time"

	"dagger/ci/internal/dagger"
	"dagger/ci/release"
)

// SkeOperator is the ske-operator component, scoped to a source checkout.
// Construct it via Ci.SkeOperator; see ci.go for CLI usage.
type SkeOperator struct {
	Checkout
}

// Build builds the ske-operator Docker image directly from its Dockerfile
// using Dagger's native BuildKit, not 'make docker-build'. The Makefile
// target shells out to the docker CLI against a local daemon, which doesn't
// exist in a plain golang container — that gap was the actual "DIND unknown"
// blocking this function, not the Kind/system-test side. Building natively
// needs no daemon at all, so Build stays fully hermetic; only SystemTest
// needs DIND, for the Kind cluster.
//
// It returns the image as a typed *Container — a content-addressed artifact
// that Dagger caches by input hash. Pass this directly to SystemTest or a
// release pipeline; the image is never rebuilt unless the source changes.
//
// CRD manifests and generated deepcopy code are committed to the repo, so no
// 'make manifests generate' step is needed before building the image.
func (m *SkeOperator) Build() *dagger.Container {
	return m.Source.DockerBuild()
}

// Unit runs ske-operator unit tests in a Go container. Caches Go modules,
// build artefacts, and envtest binaries — warm runs complete in ~30s vs ~3min
// cold on GHA.
func (m *SkeOperator) Unit(ctx context.Context) (string, error) {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("ske-operator-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("ske-operator-go-build")).
		WithMountedCache("/root/.local/share/kubebuilder-envtest", dag.CacheVolume("ske-operator-envtest")).
		WithMountedDirectory("/src", m.Source).
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

// SystemTest runs the full ske-operator e2e suite inside a Docker-in-Docker
// Kind cluster. It accepts the pre-built image as a typed parameter — if you
// already called Build, Dagger returns the cached result instantly and no
// rebuild occurs.
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
// The image parameter also shows the release pipeline path: the same
// *Container from Build can be pushed to GHCR by a release function without
// any additional rebuild step.
func (m *SkeOperator) SystemTest(
	ctx context.Context,
	// image is the pre-built ske-operator *Container from Build. Pass it here
	// to reuse the cached build; omit nothing will rebuild.
	image *dagger.Container,
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
	// This is the typed artifact handoff: the image built in Build arrives
	// here as a *File without re-running a single build step. In GHA, the
	// equivalent would require pushing to a registry first.
	imageTar := image.AsTarball()

	ctr := dag.Container().
		From("golang:1.26-bookworm").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("ske-operator-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("ske-operator-go-build")).
		WithServiceBinding("docker", dockerd).
		WithEnvVariable("DOCKER_HOST", "tcp://docker:2375")

	return kindToolchain(ctr).
		// ske-operator's e2e also needs flux and kustomize on top of the
		// shared kind/kubectl/docker.io toolchain.
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
		WithMountedDirectory("/src", m.Source).
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

// All runs unit tests and system tests in parallel, building the operator
// image exactly once. This is the full CI pipeline in a single call.
//
// The parallel execution is idiomatic Go — no separate jobs, no needs:
// arrays, no artifact upload/download between steps. The built image flows
// as a typed value from Build to SystemTest inside the same call graph.
//
// Future: a release function would accept the same *Container from Build and
// push it to GHCR — reusing the cached build, not triggering another
// 'make docker-build'.
func (m *SkeOperator) All(
	ctx context.Context,
	githubToken *dagger.Secret,
	skeLicenseToken *dagger.Secret,
) (string, error) {
	// Build once. Dagger content-addresses this by source hash — if nothing
	// changed, subsequent calls return the cached layer instantly.
	image := m.Build()

	// Unit and system-test run concurrently. Both share the same go-mod and
	// go-build cache volumes, so whichever finishes first warms the cache
	// for the other.
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
		out, err := m.SystemTest(ctx, image, githubToken, skeLicenseToken)
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

// IsReleasable dispatches through SkeRelease — ADR0013's gate for ske-operator
// (5-day LRE soak window, GitHub Deployments API, no incident flag). That
// query isn't implemented yet (see release/release.go), so this is a phase 1
// stub: always true. Tag stays zero-value until phase 2 plumbs a commit SHA
// into these calls.
func (m *SkeOperator) IsReleasable(ctx context.Context) (bool, error) {
	rel := release.SkeRelease{
		Release:    release.Release{Component: release.Component{Name: m.Name}, Type: "ske"},
		SoakWindow: 5 * 24 * time.Hour,
	}
	return rel.IsReleasable(ctx)
}
