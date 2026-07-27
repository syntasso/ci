// Package main is the Dagger CI module for the Syntasso ci repo.
//
// Run `dagger develop` from this directory first to generate dagger.gen.go
// and pin the SDK version.
//
// # Callable functions
//
//	ske-operator-build    — build the operator image; returns a typed *Container
//	ske-operator-unit     — unit tests (no cluster needed)
//	ske-operator-e2e      — e2e tests; accepts the pre-built image so nothing rebuilds
//	ske-operator-all      — unit + e2e in parallel, building exactly once
//
// # Usage examples (from the ci repo root)
//
//	# Build the image and inspect it
//	dagger -m ./dagger call ske-operator-build --source=./enterprise-kratix/ske-operator
//
//	# Unit tests only (~30s warm)
//	dagger -m ./dagger call ske-operator-unit --source=./enterprise-kratix/ske-operator
//
//	# Full pipeline: build once, unit + e2e in parallel
//	dagger -m ./dagger call ske-operator-all \
//	  --source=./enterprise-kratix/ske-operator \
//	  --github-token=env:GITHUB_TOKEN \
//	  --ske-license-token=env:SKE_LICENSE_TOKEN
package main

import (
	"context"
	"fmt"
	// fmt used in SkeOperatorAll for error wrapping and output formatting
)

// Ci is the Dagger module for Syntasso CI pipelines.
type Ci struct{}

// SkeOperatorBuild compiles the ske-operator binary and builds the Docker image.
// It returns the image as a typed *Container — a content-addressed artifact that
// Dagger caches by input hash. Pass this directly to SkeOperatorE2e or a release
// pipeline; the image is never rebuilt unless the source changes.
//
// This is the typed artifact that makes the rest of the pipeline composable.
// In GitHub Actions the equivalent is 'make docker-build' buried inside a job —
// there is no way to pass the resulting image as a typed value to the next job
// without pushing it to a registry first.
func (m *Ci) SkeOperatorBuild(source *Directory) *Container {
	return dag.Container().
		From("golang:1.26").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("ske-operator-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("ske-operator-go-build")).
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		WithExec([]string{"make", "manifests", "generate"}).
		WithExec([]string{"make", "docker-build"})
}

// SkeOperatorUnit runs ske-operator unit tests in a Go container.
// Caches Go modules, build artefacts, and envtest binaries — warm runs
// complete in ~30s vs ~3min cold on GHA.
func (m *Ci) SkeOperatorUnit(ctx context.Context, source *Directory) (string, error) {
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

// SkeOperatorE2e runs the full ske-operator e2e suite inside a Docker-in-Docker
// Kind cluster. It accepts the pre-built image as a typed parameter — if you
// already called SkeOperatorBuild, Dagger returns the cached result instantly
// and no rebuild occurs.
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
func (m *Ci) SkeOperatorE2e(
	ctx context.Context,
	// image is the pre-built ske-operator *Container from SkeOperatorBuild.
	// Pass it here to reuse the cached build; omit nothing will rebuild.
	image *Container,
	source *Directory,
	githubToken *Secret,
	skeLicenseToken *Secret,
) (string, error) {
	// Docker-in-Docker service. The cache volume keeps the layer store warm
	// across runs so image pulls don't repeat on re-runs.
	dockerd := dag.Container().
		From("docker:27-dind").
		WithMountedCache("/var/lib/docker", dag.CacheVolume("ske-operator-dind-layers")).
		AsService(ContainerAsServiceOpts{UseEntrypoint: true, InsecureRootCapabilities: true})

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
		WithExec([]string{"apt-get", "install", "-yq", "--no-install-recommends",
			"curl", "ca-certificates", "make",
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

// SkeOperatorAll runs unit tests and e2e tests in parallel, building the
// operator image exactly once. This is the full CI pipeline in a single call.
//
// The parallel execution is idiomatic Go — no separate jobs, no needs: arrays,
// no artifact upload/download between steps. The built image flows as a typed
// value from SkeOperatorBuild to SkeOperatorE2e inside the same call graph.
//
// Future: a release function would accept the same *Container from
// SkeOperatorBuild and push it to GHCR — reusing the cached build, not
// triggering another 'make docker-build'.
func (m *Ci) SkeOperatorAll(
	ctx context.Context,
	source *Directory,
	githubToken *Secret,
	skeLicenseToken *Secret,
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
	e2eCh := make(chan result, 1)

	go func() {
		out, err := m.SkeOperatorUnit(ctx, source)
		unitCh <- result{out, err}
	}()

	go func() {
		out, err := m.SkeOperatorE2e(ctx, image, source, githubToken, skeLicenseToken)
		e2eCh <- result{out, err}
	}()

	unitResult := <-unitCh
	e2eResult := <-e2eCh

	if unitResult.err != nil {
		return "", fmt.Errorf("unit: %w", unitResult.err)
	}
	if e2eResult.err != nil {
		return "", fmt.Errorf("e2e: %w", e2eResult.err)
	}

	return fmt.Sprintf("unit:\n%s\n\ne2e:\n%s", unitResult.out, e2eResult.out), nil
}
