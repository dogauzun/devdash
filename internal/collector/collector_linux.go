//go:build linux

package collector

import (
	"context"
	"errors"
)

// New returns the Linux collector.
func New() Collector { return linuxCollector{} }

type linuxCollector struct{}

func (linuxCollector) Collect(context.Context) (Result, error) {
	return Result{}, errors.New("collector: not implemented")
}
