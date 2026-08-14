package main

import "context"

// Pipeline is the contract every component object in this package satisfies:
// run its unit tests, answer whether its current build is releasable. Build
// and SystemTest stay concrete per-component methods instead of interface
// methods — their signatures differ enough (Container vs File artifact,
// tokens or not, DIND or not) that forcing them into a shared shape would
// hide more than it reveals.
type Pipeline interface {
	Unit(ctx context.Context) (string, error)
	IsReleasable(ctx context.Context) (bool, error)
}

var (
	_ Pipeline = (*SkeOperator)(nil)
	_ Pipeline = (*Kratix)(nil)
	_ Pipeline = (*KratixCli)(nil)
	_ Pipeline = (*BackstageController)(nil)
)
