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

package resolver

import (
	discovery "k8s.io/api/discovery/v1"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

func (si *serviceInfo) getIPFamilyInfo(addrType discovery.AddressType) *IPFamilyInfo {
	if addrType == discovery.AddressTypeIPv6 {
		return &si.ipv6Info
	}

	return &si.ipv4Info
}

func (si *serviceInfo) isHeadless() bool {
	return si.spec.Type == mcsv1a1.Headless
}

func (si *serviceInfo) canBeDeleted() bool {
	return !si.isExported && len(si.ipv4Info.clusters) == 0 && len(si.ipv6Info.clusters) == 0
}
