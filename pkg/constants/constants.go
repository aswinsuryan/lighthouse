/*
SPDX-License-Identifier: Apache-2.0

Copyright Contributors to the Submariner project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package constants

const (
	LabelSourceNamespace     = "lighthouse.submariner.io/sourceNamespace"
	LabelValueManagedBy      = "lighthouse-agent.submariner.io"
	LabelIsHeadless          = "lighthouse.submariner.io/is-headless"
	LabelSourceName          = "lighthouse.submariner.io/source-name"
	PublishNotReadyAddresses = "lighthouse.submariner.io/publish-not-ready-addresses"
	GlobalnetEnabled         = "lighthouse.submariner.io/globalnet-enabled"
	UseClustersetIP          = "lighthouse.submariner.io/use-clusterset-ip"
	ClustersetIPAllocatedBy  = "lighthouse.submariner.io/clusterset-ip-allocated-by"
)

const ServiceExportReady = "Ready"
