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

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

func fetch(client *http.Client, url string) ([]byte, map[string]*dto.MetricFamily, error) {
	response, err := client.Get(url)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s: HTTP %d", url, response.StatusCode)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, err
	}
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(bytes.NewReader(data))
	return data, families, err
}

func labels(metric *dto.Metric) map[string]string {
	result := make(map[string]string, len(metric.GetLabel()))
	for _, pair := range metric.GetLabel() {
		result[pair.GetName()] = pair.GetValue()
	}
	return result
}

func matchesContainer(values map[string]string, pod, container, namespace string) bool {
	return values["container_host"] == pod && values["container_name"] == container &&
		values["container_hostnamespace"] == namespace
}

func validateLabels(actual, expected map[string]string) error {
	if len(actual) != len(expected) {
		return fmt.Errorf("labels=%v, want %v", actual, expected)
	}
	for name, want := range expected {
		if value, ok := actual[name]; !ok || value != want {
			return fmt.Errorf("label %s=%q, want %q", name, value, want)
		}
	}
	return nil
}

func validateCollector(families map[string]*dto.MetricFamily, collector string) error {
	count := 0
	for _, metric := range families["huatuo_bamai_scrape_collector_success"].GetMetric() {
		if labels(metric)["collector"] != collector {
			continue
		}
		count++
		if metric.GetGauge().GetValue() != 1 {
			return fmt.Errorf("%s scrape_collector_success is not 1", collector)
		}
	}
	if count != 1 {
		return fmt.Errorf("expected one %s scrape success, got %d", collector, count)
	}
	return nil
}
