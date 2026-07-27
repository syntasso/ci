// Package main is the Dagger CI module for the Syntasso ci repo.
//
// Run `dagger develop` from this directory first to generate dagger.gen.go
// and pin the SDK version.
//
// Usage examples (from the ci repo root):
//
//	# Unit tests only — fast, no cluster needed
//	dagger -m ./dagger call ske-operator-unit --source=<path-to>/enterprise-kratix/ske-operator
//
//	# Full e2e — spins up Kind via Docker-in-Docker
//	dagger -m ./dagger call ske-operator-e2e \
//	  --source=<path-to>/enterprise-kratix/ske-operator \
//	  --github-token=env:GITHUB_TOKEN \
//	  --ske-license-token=env:SKE_LICENSE_TOKEN
package main

import (
	"context"
)

// Ci is the Dagger module for Syntasso CI pipelines.
type Ci struct{}

// SkeOperatorUnit runs ske-operator unit tests inside a Go container.
//
// Replicates the unit-test job from test-ske-operator.yaml.
// Caches Go modules, build artefacts, and envtest binaries across runs —
// subsequent runs complete in ~30s vs ~3min cold.
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

// SkeOperatorE2e runs the ske-operator e2e test suite inside a Docker-in-Docker
// container, creating a Kind cluster from scratch.
//
// Replicates the e2e-test job from test-ske-operator.yaml, with three key
// improvements over the GHA version:
//
//  1. cert-manager images are pre-pulled and loaded into Kind before anything
//     starts, eliminating the cold-pull window that causes the webhook cert
//     timing race.
//
//  2. cert-manager readiness is explicitly waited on (all three deployments
//     Available) before the ske-operator is deployed — the step that
//     previously caused CrashLoopBackOff when the webhook cert wasn't ready.
//
//  3. The entire cluster lives in a Dagger DIND service, so runner variance
//     (AZ exhaustion, Sprinters InternalError) doesn't affect cluster state.
//
// Spike status: unit test path validated locally. E2e DIND wiring is
// structurally complete but needs a live run to confirm Kind + DOCKER_HOST
// interaction with Dagger's engine. See PR description for known unknowns.
func (m *Ci) SkeOperatorE2e(
	ctx context.Context,
	source *Directory,
	githubToken *Secret,
	skeLicenseToken *Secret,
) (string, error) {
	// Docker-in-Docker service. The cache volume keeps the Docker layer store
	// warm across runs so image pulls don't repeat.
	dockerd := dag.Container().
		From("docker:27-dind").
		WithMountedCache("/var/lib/docker", dag.CacheVolume("ske-operator-dind-layers")).
		AsService(ContainerAsServiceOpts{UseEntrypoint: true, InsecureRootCapabilities: true})

	return dag.Container().
		From("golang:1.26-bookworm").
		// Go caches
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("ske-operator-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("ske-operator-go-build")).
		// Wire up Docker daemon
		WithServiceBinding("docker", dockerd).
		WithEnvVariable("DOCKER_HOST", "tcp://docker:2375").
		// Install system tools
		WithExec([]string{"apt-get", "update", "-qq"}).
		WithExec([]string{"apt-get", "install", "-yq", "--no-install-recommends",
			"curl", "wget", "ca-certificates", "make",
		}).
		// kind
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/v0.25.0/kind-linux-amd64 \
			 && chmod +x /usr/local/bin/kind`,
		}).
		// kubectl
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/kubectl \
			   "https://dl.k8s.io/release/v1.30.0/bin/linux/amd64/kubectl" \
			 && chmod +x /usr/local/bin/kubectl`,
		}).
		// flux (pinned — same version as GHA workflow)
		WithExec([]string{"sh", "-c",
			`curl -s https://fluxcd.io/install.sh | FLUX_VERSION=2.4.0 bash`,
		}).
		// kustomize
		WithExec([]string{"sh", "-c",
			`curl -sSL "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh" \
			 | bash -s -- /usr/local/bin`,
		}).
		// Source
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		// Pre-pull cert-manager images in parallel before the cluster exists.
		// When Kind loads these images they're already in the Docker layer cache —
		// no in-cluster pull required, eliminating the cold-pull timing window.
		WithExec([]string{"sh", "-c", `
			docker pull quay.io/jetstack/cert-manager-controller:v1.15.3 &
			docker pull quay.io/jetstack/cert-manager-webhook:v1.15.3 &
			docker pull quay.io/jetstack/cert-manager-cainjector:v1.15.3 &
			wait
		`}).
		// Create Kind cluster
		WithExec([]string{"kind", "create", "cluster", "--name", "platform"}).
		// Load pre-pulled cert-manager images into the Kind node image store.
		// This means cert-manager pods start immediately with no pull latency.
		WithExec([]string{"sh", "-c", `
			kind load docker-image quay.io/jetstack/cert-manager-controller:v1.15.3 --name platform &
			kind load docker-image quay.io/jetstack/cert-manager-webhook:v1.15.3 --name platform &
			kind load docker-image quay.io/jetstack/cert-manager-cainjector:v1.15.3 --name platform &
			wait
		`}).
		// Configure SKE namespace and registry secret
		WithExec([]string{
			"kubectl", "--context", "kind-platform",
			"create", "namespace", "kratix-platform-system",
		}).
		WithExec([]string{
			"kubectl", "--context", "kind-platform",
			"apply", "-f", "config/samples/config.yaml",
		}).
		// Build and load the ske-operator image
		WithExec([]string{"make", "docker-build"}).
		WithExec([]string{"sh", "-c",
			`kind load docker-image "${REGISTRY:-ghcr.io/syntasso}/${PACKAGE_NAME:-ske-operator}:${VERSION:-v0.0.1}" --name platform`,
		}).
		// Install CRDs
		WithExec([]string{"sh", "-c", `kustomize build config/crd | kubectl apply -f -`}).
		// Install cert-manager
		WithExec([]string{
			"kubectl", "apply", "-f",
			"https://github.com/cert-manager/cert-manager/releases/download/v1.15.3/cert-manager.yaml",
		}).
		// Wait for all three cert-manager deployments to be Available before
		// deploying anything that needs the webhook cert. This is the explicit
		// gate that the original GHA workflow lacked, causing CrashLoopBackOff.
		WithExec([]string{
			"kubectl", "wait", "deployment/cert-manager",
			"--for=condition=Available", "--timeout=120s", "-n", "cert-manager",
		}).
		WithExec([]string{
			"kubectl", "wait", "deployment/cert-manager-webhook",
			"--for=condition=Available", "--timeout=120s", "-n", "cert-manager",
		}).
		WithExec([]string{
			"kubectl", "wait", "deployment/cert-manager-cainjector",
			"--for=condition=Available", "--timeout=120s", "-n", "cert-manager",
		}).
		// Run e2e tests
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
