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
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	profilev1 "github.com/grafana/pyroscope/api/gen/proto/go/google/v1"
)

func TestServiceReadyRejectsUninitializedStorage(t *testing.T) {
	err := (*Service)(nil).Ready(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("Ready() error = %v, want initialization error", err)
	}
}

func TestApplyProfileMatcherRegion(t *testing.T) {
	filter := &SearchFilter{}
	matcher := &labels.Matcher{Name: "region", Value: "cn-beijing", Type: labels.MatchEqual}

	if err := applyProfileMatcher(filter, matcher); err != nil {
		t.Fatalf("applyProfileMatcher() error = %v", err)
	}
	if filter.Region != "cn-beijing" {
		t.Errorf("filter.Region = %q, want %q", filter.Region, "cn-beijing")
	}
}

func TestApplyProfileMatcherRejectsUnknownLabel(t *testing.T) {
	filter := &SearchFilter{}
	matcher := &labels.Matcher{Name: "unknown", Value: "x", Type: labels.MatchEqual}

	if err := applyProfileMatcher(filter, matcher); err == nil {
		t.Fatal("applyProfileMatcher() error = nil, want error for unknown label")
	}
}

func TestProfileStringRejectsInvalidIndex(t *testing.T) {
	table := []string{"", "samples"}
	if got, ok := profileString(table, 1); !ok || got != "samples" {
		t.Fatalf("profileString(1)=(%q,%t), want (samples,true)", got, ok)
	}
	for _, index := range []int64{-1, 2, 100} {
		if got, ok := profileString(table, index); ok || got != "" {
			t.Errorf("profileString(%d)=(%q,%t), want empty,false", index, got, ok)
		}
	}
}

func TestBuildProfileSearchQueryIncludesPage(t *testing.T) {
	query := buildProfileSearchQuery(&SearchFilter{TracerID: "task-2026", Limit: 25, Offset: 50})
	if query.Limit != 25 || query.Offset != 50 {
		t.Fatalf("query page=(%d,%d), want (25,50)", query.Limit, query.Offset)
	}
}

// minimalIrqProfile builds a well-formed irq count profile with one frame-
// less sample: enough for mergeStacktraces to merge and convert without
// depending on real profile content.
func minimalIrqProfile() profilev1.Profile {
	return profilev1.Profile{
		SampleType:  []*profilev1.ValueType{{Type: 0, Unit: 1}},
		PeriodType:  &profilev1.ValueType{Type: 1, Unit: 1},
		Sample:      []*profilev1.Sample{{Value: []int64{5}}},
		StringTable: []string{"irq", "count"},
	}
}

// decodeTracerData builds a ProfileDocument by decoding the exact JSON
// shape the storage layer produces, so the test also locks the "SearchProfiles
// decode keeps nmissed" contract.
func decodeTracerData(t *testing.T, tracerData any) *ProfileDocument {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"tracer_data": tracerData})
	if err != nil {
		t.Fatalf("marshal tracer data: %v", err)
	}

	document, err := (profileDocumentMapper{}).Decode(raw)
	if err != nil {
		t.Fatalf("decode tracer data: %v", err)
	}
	return document
}

func irqDocument(t *testing.T, nmissed uint64) *ProfileDocument {
	t.Helper()

	profile := minimalIrqProfile()
	return decodeTracerData(t, map[string]any{
		"flamedata": map[string]any{
			"profile_type": "irq_tracing:irq:count:irq:count",
			"profile":      &profile,
		},
		"nmissed": nmissed,
	})
}

// TestProfileDocumentDecodeKeepsNMissed locks the decode contract: the
// profileDocumentMapper must not drop the nmissed field from tracer_data.
func TestProfileDocumentDecodeKeepsNMissed(t *testing.T) {
	document := irqDocument(t, 1234)
	if document.TracerData.NMissed != 1234 {
		t.Fatalf("TracerData.NMissed = %d, want 1234", document.TracerData.NMissed)
	}

	// Documents without the field (all other tracers, older documents)
	// decode to zero and must not change behavior.
	profile := minimalIrqProfile()
	legacy := decodeTracerData(t, map[string]any{
		"flamedata": map[string]any{"profile_type": "irq_tracing:irq:count:irq:count", "profile": &profile},
	})
	if legacy.TracerData.NMissed != 0 {
		t.Fatalf("legacy TracerData.NMissed = %d, want 0", legacy.TracerData.NMissed)
	}
}

// TestMergeStacktracesToleratesIncomplete verifies that a truncated profile is
// merged into the flame graph with only a warning: failing the query would
// make every flame graph query touching a budgeted tracer unusable.
func TestMergeStacktracesToleratesIncomplete(t *testing.T) {
	t.Run("single incomplete document still merges", func(t *testing.T) {
		resp, err := mergeStacktraces([]*ProfileDocument{irqDocument(t, 7)}, "irq")
		if err != nil {
			t.Fatalf("mergeStacktraces() error = %v, want nil", err)
		}
		if resp == nil || resp.Flamegraph == nil {
			t.Fatal("mergeStacktraces() response lacks a flame graph")
		}
	})

	t.Run("multiple incomplete documents still merge", func(t *testing.T) {
		resp, err := mergeStacktraces([]*ProfileDocument{
			irqDocument(t, 3),
			irqDocument(t, 4),
		}, "irq")
		if err != nil {
			t.Fatalf("mergeStacktraces() error = %v, want nil", err)
		}
		if resp == nil || resp.Flamegraph == nil {
			t.Fatal("mergeStacktraces() response lacks a flame graph")
		}
	})

	t.Run("complete documents merge successfully", func(t *testing.T) {
		resp, err := mergeStacktraces([]*ProfileDocument{
			irqDocument(t, 0),
			irqDocument(t, 0),
		}, "irq")
		if err != nil {
			t.Fatalf("mergeStacktraces() error = %v, want nil", err)
		}
		if resp == nil || resp.Flamegraph == nil {
			t.Fatal("mergeStacktraces() response lacks a flame graph")
		}
	})
}

// TestMergeStacktracesUnrelatedErrorIsNotIncomplete guards the negative case:
// a merge failure without any dropped samples must not be reported as an
// incomplete profile.
func TestMergeStacktracesUnrelatedErrorIsNotIncomplete(t *testing.T) {
	document := irqDocument(t, 0)
	document.TracerData.Flamedata.Profile.StringTable = nil

	_, err := mergeStacktraces([]*ProfileDocument{document}, "irq")
	if err == nil {
		t.Fatal("mergeStacktraces() error = nil, want merge failure")
	}
	if strings.Contains(err.Error(), "incomplete profile") {
		t.Fatalf("error %q misreports an incomplete profile", err)
	}
}
