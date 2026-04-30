// Copyright (c) 2025 Dmitrij Shishkin (greeddj@gmail.com)
// SPDX-License-Identifier: MIT

package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/defaults"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func staticBoolDefault(value bool) defaults.Bool {
	return staticBoolDefaultValue{value: value}
}

type staticBoolDefaultValue struct {
	value bool
}

func (d staticBoolDefaultValue) Description(_ context.Context) string {
	if d.value {
		return "defaults to true"
	}
	return "defaults to false"
}

func (d staticBoolDefaultValue) MarkdownDescription(ctx context.Context) string {
	return d.Description(ctx)
}

func (d staticBoolDefaultValue) DefaultBool(_ context.Context, _ defaults.BoolRequest, resp *defaults.BoolResponse) {
	resp.PlanValue = types.BoolValue(d.value)
}
