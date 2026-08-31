// Copyright 2026 The HuaTuo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"reflect"
	"testing"

	"huatuo-bamai/internal/storage/driver"
)

func TestBuildProfileAggregationQueryPreservesTargetMatchers(t *testing.T) {
	query := buildProfileAggregationQuery(&SearchFilter{
		ContainerID:       "containerd://4df60fc5",
		ContainerHostname: "checkout-api-7b9f6d8c4f-k2x7m",
	})
	want := []driver.Filter{
		{
			Field: profileFieldContainerID + ".keyword",
			Op:    driver.OpEq,
			Value: "containerd://4df60fc5",
		},
		{
			Field: profileFieldContainerHostname + ".keyword",
			Op:    driver.OpEq,
			Value: "checkout-api-7b9f6d8c4f-k2x7m",
		},
	}

	if !reflect.DeepEqual(query.Filters, want) {
		t.Fatalf("query filters = %#v, want %#v", query.Filters, want)
	}
}

func TestBuildProfileAggregationQueryAddsTracerIDOnce(t *testing.T) {
	query := buildProfileAggregationQuery(&SearchFilter{TracerID: "task-20260722-8f6a"})
	want := []driver.Filter{
		{
			Field: profileFieldTracerID + ".keyword",
			Op:    driver.OpEq,
			Value: "task-20260722-8f6a",
		},
	}

	if !reflect.DeepEqual(query.Filters, want) {
		t.Fatalf("query filters = %#v, want %#v", query.Filters, want)
	}
}

func TestNormalizeProfileAggregationFieldUsesKeywordVariant(t *testing.T) {
	tests := []struct {
		name  string
		field string
		want  string
	}{
		{name: "id", field: "id", want: profileFieldTracerID + ".keyword"},
		{name: "profile type", field: profileFieldProfileType, want: profileFieldProfileType + ".keyword"},
		{name: "region", field: profileFieldRegion, want: profileFieldRegion + ".keyword"},
		{name: "hostname", field: profileFieldHostname, want: profileFieldHostname + ".keyword"},
		{name: "tracer name", field: profileFieldTracerName, want: profileFieldTracerName + ".keyword"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeProfileAggregationField(tt.field)
			if err != nil {
				t.Fatalf("normalizeProfileAggregationField(%q): %v", tt.field, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeProfileAggregationField(%q) = %q, want %q", tt.field, got, tt.want)
			}
		})
	}
}

func TestNormalizeProfileAggregationFieldRejectsUnknown(t *testing.T) {
	if _, err := normalizeProfileAggregationField("flamedata"); err == nil {
		t.Fatal("normalizeProfileAggregationField accepted an unknown field")
	}
}

func TestBuildProfileAggregationQueryFiltersByRegion(t *testing.T) {
	query := buildProfileAggregationQuery(&SearchFilter{
		Region:   "cn-beijing",
		Hostname: "node-1",
	})

	found := false
	for _, f := range query.Filters {
		if f.Field == profileFieldRegion+".keyword" && f.Value == "cn-beijing" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("query filters = %#v, want region filter present", query.Filters)
	}
}
