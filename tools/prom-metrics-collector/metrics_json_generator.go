/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.  * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2023 Red Hat, Inc.
 *
 */

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kubevirt/monitoring/pkg/metrics/parser"

	"kubevirt.io/hostpath-provisioner-operator/pkg/monitoring/metrics"
)

// This should be used only for very rare cases where the naming conventions that are explained in the best practices:
// https://sdk.operatorframework.io/docs/best-practices/observability-best-practices/#metrics-guidelines
// should be ignored.
var excludedMetrics = map[string]struct{}{}

func main() {
	err := metrics.SetupMetrics()
	if err != nil {
		panic(err)
	}

	var metricFamilies []parser.Metric

	metricsList := metrics.ListMetrics()
	for _, m := range metricsList {
		if _, isExcludedMetric := excludedMetrics[m.GetOpts().Name]; !isExcludedMetric {
			metricFamilies = append(metricFamilies, parser.Metric{
				Name: m.GetOpts().Name,
				Help: m.GetOpts().Help,
				Type: strings.ToUpper(string(m.GetBaseType())),
			})
		}
	}

	// Add the recordingrule rules manually until refactor
	recordingrule := parser.Metric{
		Name: "kubevirt_hpp_operator_up",
		Help: "The number of running hostpath-provisioner-operator pods",
		Type: "GAUGE",
	}
	metricFamilies = append(metricFamilies, recordingrule)

	jsonBytes, err := json.Marshal(metricFamilies)
	if err != nil {
		panic(err)
	}

	fmt.Println(string(jsonBytes)) // Write the JSON string to standard output
}
