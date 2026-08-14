package main

import "dagger/ci/internal/dagger"

// kindToolchain installs the CLI tools a container needs to drive a Kind
// cluster over a Docker-in-Docker service: docker.io provides the docker
// CLI client used to talk to the DIND daemon over DOCKER_HOST; kind and
// kubectl are fetched as pinned static binaries. SkeOperator.SystemTest and
// Kratix.SystemTest/Platform installed byte-for-byte identical commands for
// this before extraction — real, already-existing duplication, not
// hypothetical.
func kindToolchain(ctr *dagger.Container) *dagger.Container {
	return ctr.
		WithExec([]string{"apt-get", "update", "-qq"}).
		WithExec([]string{"apt-get", "install", "-yq", "--no-install-recommends",
			"curl", "ca-certificates", "make", "docker.io",
		}).
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/v0.25.0/kind-linux-amd64 && chmod +x /usr/local/bin/kind`,
		}).
		WithExec([]string{"sh", "-c",
			`curl -sSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/v1.30.0/bin/linux/amd64/kubectl" && chmod +x /usr/local/bin/kubectl`,
		})
}
