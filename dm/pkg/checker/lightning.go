// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package checker

import (
	"context"

	"github.com/pingcap/tidb/br/pkg/lightning/restore"
)

func convertLightningPrecheck(
	ctx context.Context,
	dmResult *Result,
	lightningPrechecker restore.PrecheckItem,
	failLevel State,
) {
	lightningResult, err := lightningPrechecker.Check(ctx)
	if err != nil {
		markCheckError(dmResult, err)
		return
	}
	if !lightningResult.Passed {
		dmResult.State = failLevel
		dmResult.Errors = append(dmResult.Errors, &Error{Severity: failLevel, ShortErr: lightningResult.Message})
		return
	}
	dmResult.State = StateSuccess
}

// LightningEmptyRegionChecker checks whether there are too many empty regions in the cluster.
type LightningEmptyRegionChecker struct {
	inner restore.PrecheckItem
}

// NewLightningEmptyRegionChecker creates a new LightningEmptyRegionChecker.
func NewLightningEmptyRegionChecker(lightningChecker restore.PrecheckItem) RealChecker {
	return &LightningEmptyRegionChecker{inner: lightningChecker}
}

// Name implements the RealChecker interface.
func (c *LightningEmptyRegionChecker) Name() string {
	return "lightning_empty_region"
}

// Check implements the RealChecker interface.
func (c *LightningEmptyRegionChecker) Check(ctx context.Context) *Result {
	result := &Result{
		Name:  c.Name(),
		Desc:  "check downstream cluster has too many empty regions",
		State: StateFailure,
	}
	convertLightningPrecheck(ctx, result, c.inner, StateWarning)
	return result
}

// LightningRegionDistributionChecker checks whether the region distribution is balanced.
type LightningRegionDistributionChecker struct {
	inner restore.PrecheckItem
}

// NewLightningRegionDistributionChecker creates a new LightningRegionDistributionChecker.
func NewLightningRegionDistributionChecker(lightningChecker restore.PrecheckItem) RealChecker {
	return &LightningRegionDistributionChecker{inner: lightningChecker}
}

// Name implements the RealChecker interface.
func (c *LightningRegionDistributionChecker) Name() string {
	return "lightning_region_distribution"
}

// Check implements the RealChecker interface.
func (c *LightningRegionDistributionChecker) Check(ctx context.Context) *Result {
	result := &Result{
		Name:  c.Name(),
		Desc:  "check downstream cluster region distribution",
		State: StateFailure,
	}
	convertLightningPrecheck(ctx, result, c.inner, StateWarning)
	return result
}

// LightningClusterVersionChecker checks whether the cluster version is compatible with Lightning.
type LightningClusterVersionChecker struct {
	inner restore.PrecheckItem
}

// NewLightningClusterVersionChecker creates a new LightningClusterVersionChecker.
func NewLightningClusterVersionChecker(lightningChecker restore.PrecheckItem) RealChecker {
	return &LightningClusterVersionChecker{inner: lightningChecker}
}

// Name implements the RealChecker interface.
func (c *LightningClusterVersionChecker) Name() string {
	return "lightning_cluster_version"
}

// Check implements the RealChecker interface.
func (c *LightningClusterVersionChecker) Check(ctx context.Context) *Result {
	result := &Result{
		Name:  c.Name(),
		Desc:  "check downstream cluster version",
		State: StateFailure,
	}
	convertLightningPrecheck(ctx, result, c.inner, StateFailure)
	return result
}
