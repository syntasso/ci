# Dagger CI pattern: findings

Survey done while extending the Dagger CI module in this directory to more
than one component. Two questions: does the same Build/Unit/SystemTest/
IsReleasable shape actually hold across genuinely different toolchains, and
how far across the estate could this reasonably reach.

## Components wired up on this branch

| | ske-operator | kratix | kratix-cli | backstage-controller |
|---|---|---|---|---|
| Embeds `Checkout` | Yes | Yes | Yes | Yes |
| Satisfies `Pipeline` | Yes | Yes | Yes | Yes |
| `Build()` | native `DockerBuild()` | native `DockerBuild()` | plain `go build` → `*File` | native `DockerBuild()` |
| `Unit()` | envtest, hardcoded `1.30.0` | ginkgo, no envtest | ginkgo + bash fixture script | envtest, computed from `go.mod` |
| `SystemTest()` | DIND + Kind, hand-built | DIND + Kind, via `Platform()` | — no k8s dependency | — needs kratix installed, not yet wired |
| `IsReleasable()` | via `SkeRelease` (gate stubbed, phase 2) | via `KratixRelease` (no gate) | via base `Release` (no dedicated ADR type) | via base `Release` (no dedicated ADR type) |

Verified live, locally, against the real component repos: all four `Build`s,
`ske-operator-unit`, `kratix-unit`, `is-releasable` on all four,
`backstage-controller-unit`. `SystemTest`/`Platform` are cloud-only by design
(~31min, reconciliation-bound) and not verified locally — that's a disclosed
gap, not an oversight.

## Full survey: every component in enterprise-kratix

| Component | Language | Dockerfile? | Test framework | Envtest? | System-test prerequisite | Release mechanism |
|---|---|---|---|---|---|---|
| ske-operator *(wired)* | Go | Yes | go test/ginkgo | hardcoded `1.30.0` | needs kratix installed | release-please |
| backstage-controller *(wired)* | Go | Yes | go test/ginkgo | computed from `go.mod` | needs kratix + registry secret | release-please |
| ske-cortex-controller | Go | Yes | go test + ginkgo (+ separate Chromium UI e2e) | computed via custom `gomodver` macro | needs kratix + registry secret | release-please |
| ske-portal-controller | Go | Yes, via `ci/scripts/docker-build.sh` wrapper | ginkgo, chains into nested sub-tests | computed (`gomodver`) | full SKE substrate on Kind | release-please |
| ↳ portal-patch (own go.mod) | Go | Yes, own Dockerfile | plain `go test` | no — not a controller | none | release-please, **separate** component |
| ske-platform-manager | Go | Yes | ginkgo | **none at all** | whole-platform quick-start | **not release-please** — bundled into the overall SKE distribution tag |
| ske-quick-start-installer | Go | no own Dockerfile (root-level, cross-repo build context) | **no unit test target** | no | full-stack build+load, incl. backstage | **not release-please** — same bundled-tag mechanism |
| headlamp-ske | **Node** | Yes (mixed Go+Node stages) | Jest | n/a | none dedicated | release-please |
| cli-plugins/kratix-test | Go | **no Dockerfile** — cross-compiled binary | ginkgo | no | assumes pre-existing kratix context | release-please, separate component |
| k8s-health-agent | Go | Yes | go test/ginkgo | hardcoded `1.31.0` | needs kratix installed | release-please |
| ske-cli | — | — | — | — | — | **not a real component** — checked-in prebuilt binary, no source |

Excluded after inspection: `src/` (empty), `promises/` (Kratix Promise YAML,
not a built/released service), `tests/` (shared e2e/soak harness, no
independent release), `read-only-kratix` (pinned OSS submodule).

**Fit assessment:** `ske-cortex-controller`, `ske-portal-controller`,
`k8s-health-agent`, `headlamp-ske`, `portal-patch`, and
`cli-plugins/kratix-test` all fit the same Build/Unit/SystemTest/IsReleasable
shape cleanly — `headlamp-ske` needs a Node-flavoured implementation of the
interface rather than Go, but the shape holds. `ske-platform-manager` and
`ske-quick-start-installer` genuinely don't fit `IsReleasable`: their version
is inherited from the overall SKE distribution tag, not a per-component
release-please entry, so "is this releasable" isn't a per-component question
for them. `ske-cli` doesn't fit at all — there's no source to wrap.

## Bugs found and fixed while doing this

1. `SkeOperatorBuild` shelled out to `make docker-build`, which needs a
   docker daemon a plain container doesn't have. Fixed by switching to
   Dagger's native `Directory.DockerBuild()` — no daemon needed.
2. The DIND system-test container was missing the `docker.io` package
   entirely, so `docker load`/`docker pull` could never have run.
3. `BackstageController.Unit` hardcoded a snapshot (`1.36`) of what its own
   Makefile computes dynamically from `go.mod` — silent drift risk the
   moment `k8s.io/api` gets bumped. Fixed by computing it for real.

## Centralisation done, not just proposed

- `kind_toolchain.go` — `ske-operator` and `kratix`'s `SystemTest` had
  byte-for-byte identical kind/kubectl/docker.io install steps. One shared
  function now, both use it.
- `envtest.go` — `envtestK8sVersion()` replicates the `go list -m k8s.io/api`
  computation `backstage-controller`'s Makefile does (and, separately,
  `ske-cortex-controller`/`ske-portal-controller`'s Makefiles do via a
  duplicated custom macro) — one shared implementation for any component
  computing it this way.
- `Kratix.Platform()` — extracted the "bring up a running kratix platform"
  logic out of `Kratix.SystemTest` into its own function, returning the
  running platform as a `*dagger.Service`. This is the piece
  `ske-cortex-controller`, `ske-portal-controller`, and `k8s-health-agent`
  each need as their own e2e prerequisite today (each reimplementing it
  independently) — ready for when they're wrapped, dogfooded immediately by
  `Kratix.SystemTest` itself.
