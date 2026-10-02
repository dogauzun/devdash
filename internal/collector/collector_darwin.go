//go:build darwin

package collector

import (
	"context"
	"errors"
)

// New returns the macOS collector.
func New() Collector { return darwinCollector{} }

type darwinCollector struct{}

func (darwinCollector) Collect(context.Context) (Result, error) {
	return Result{}, errors.New("collector: not implemented")
}
