package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/ci/internal/dagger"
)

// envtestK8sVersion computes the Kubernetes version to hand to setup-envtest,
// derived from the k8s.io/api version the module actually depends on — the
// same approach backstage-controller's, ske-cortex-controller's, and
// ske-portal-controller's Makefiles each take today, via three separate
// mechanisms (one inline `go list -m` computation, two copies of a custom
// `gomodver` macro). This is the one shared implementation: any component
// whose Makefile computes its envtest version this way should call this
// instead of hardcoding a snapshot of the answer, which silently drifts the
// moment go.mod bumps k8s.io/api.
//
// ske-operator's own Makefile hardcodes ENVTEST_K8S_VERSION as a literal
// (1.30.0) rather than computing it — that's a real difference in that
// component's own build config, not something this function should paper
// over by forcing computation where the source of truth doesn't compute.
func envtestK8sVersion(ctx context.Context, source *dagger.Directory) (string, error) {
	out, err := dag.Container().
		From("golang:1.26").
		WithMountedDirectory("/src", source).
		WithWorkdir("/src").
		WithExec([]string{"go", "list", "-m", "-f", "{{ .Version }}", "k8s.io/api"}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve k8s.io/api version: %w", err)
	}

	// k8s.io/api versions as "v0.MINOR.PATCH", corresponding to Kubernetes
	// "1.MINOR" — e.g. "v0.36.2" -> "1.36". Same mapping the Makefiles'
	// `awk -F'[v.]' '{printf "1.%d", $3}'` performs.
	v := strings.TrimPrefix(strings.TrimSpace(out), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 || parts[1] == "" {
		return "", fmt.Errorf("unexpected k8s.io/api version format: %q", out)
	}
	return "1." + parts[1], nil
}
