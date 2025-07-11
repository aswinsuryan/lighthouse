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
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/submariner-io/admiral/pkg/resource"
	"github.com/submariner-io/lighthouse/coredns/constants"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	k8snet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

const maxRecordsToLog = 5

func (i *Interface) PutEndpointSlices(endpointSlices ...*discovery.EndpointSlice) bool {
	if len(endpointSlices) == 0 {
		return false
	}

	key, clusterID, ok := getKeyInfoFrom(endpointSlices[0])
	if !ok {
		return false
	}

	logger.Infof("Put %s EndpointSlices for %q on cluster %q", endpointSlices[0].AddressType, key, clusterID)

	localClusterID := i.clusterStatus.GetLocalClusterID()

	var (
		localEndpointSliceErr error
		localEndpointSlices   []*discovery.EndpointSlice
	)

	if localClusterID != "" && clusterID == localClusterID && shouldRetrieveLocalEndpointSlicesFor(endpointSlices[0]) {
		// The EndpointSlice is from the local cluster. With globalnet enabled, the local global endpoint IPs aren't
		// routable in the local cluster so we retrieve the K8s EndpointSlice and use those endpoints. Note that this
		// only applies to headless services.
		localEndpointSlices, localEndpointSliceErr = i.getLocalEndpointSlices(endpointSlices[0])
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	svcInfo, found := i.serviceMap[key]
	if !found {
		// This means we haven't observed a ServiceImport yet for the service. Return true for the controller to re-queue it.
		logger.Infof("Service not found for EndpointSlice %q - requeuing", key)

		return true
	}

	ipFamilyInfo := svcInfo.getIPFamilyInfo(endpointSlices[0].AddressType)

	if !svcInfo.isHeadless() {
		return i.putClusterIPEndpointSlice(key, clusterID, endpointSlices[0], ipFamilyInfo)
	}

	if localEndpointSliceErr != nil {
		logger.Error(localEndpointSliceErr, "unable to retrieve local EndpointSlice - requeuing")

		return true
	}

	if localEndpointSlices != nil {
		endpointSlices = localEndpointSlices
	}

	i.putHeadlessEndpointSlices(key, clusterID, endpointSlices, ipFamilyInfo)

	return false
}

func (i *Interface) putClusterIPEndpointSlice(key, clusterID string, endpointSlice *discovery.EndpointSlice, ipFamilyInfo *IPFamilyInfo,
) bool {
	_, found := endpointSlice.Labels[constants.LabelIsHeadless]
	if !found {
		// This is a legacy pre-0.15 EndpointSlice.
		clusterInfo, found := ipFamilyInfo.clusters[clusterID]
		if !found {
			logger.Infof("Cluster %q not found for EndpointSlice %q - requeuing", clusterID, key)
			return true
		}

		// For a ClusterIPService we really only care if there are any backing endpoints.
		clusterInfo.endpointsHealthy = len(endpointSlice.Endpoints) > 0

		return false
	}

	if len(endpointSlice.Endpoints) == 0 {
		// This shouldn't happen - we expect the service IP endpoint to always be present.
		logger.Errorf(nil, "Missing service IP endpoint in EndpointSlice %q", key)

		return false
	}

	clusterInfo := ipFamilyInfo.ensureClusterInfo(clusterID)
	clusterInfo.endpointRecords = []DNSRecord{{
		IP:          endpointSlice.Endpoints[0].Addresses[0],
		Ports:       mcsServicePortsFrom(endpointSlice.Ports),
		ClusterName: clusterID,
	}}

	clusterInfo.endpointsHealthy = endpointSlice.Endpoints[0].Conditions.Ready == nil || *endpointSlice.Endpoints[0].Conditions.Ready

	ipFamilyInfo.mergePorts()
	ipFamilyInfo.resetLoadBalancing()

	logger.Infof("Added %s DNSRecord with service IP %q for EndpointSlice %q on cluster %q, endpointsHealthy: %v, ports: %#v",
		endpointSlice.AddressType, clusterInfo.endpointRecords[0].IP, key, clusterID, clusterInfo.endpointsHealthy,
		clusterInfo.endpointRecords[0].Ports)

	return false
}

func (i *Interface) putHeadlessEndpointSlices(key, clusterID string, endpointSlices []*discovery.EndpointSlice,
	ipFamilyInfo *IPFamilyInfo,
) {
	clusterInfo := &clusterInfo{
		endpointRecordsByHost: make(map[string][]DNSRecord),
	}

	ipFamilyInfo.clusters[clusterID] = clusterInfo

	allAddresses := sets.New[string]()

	for _, endpointSlice := range endpointSlices {
		mcsPorts := mcsServicePortsFrom(endpointSlice.Ports)
		publishNotReadyAddresses := endpointSlice.Annotations[constants.PublishNotReadyAddresses] == strconv.FormatBool(true)

		for i := range endpointSlice.Endpoints {
			endpoint := &endpointSlice.Endpoints[i]

			// Skip if not ready and the user does not want to publish not-ready addresses. Note: we're treating nil as ready
			// to be on the safe side as the EndpointConditions doc states "In most cases consumers should interpret this
			// unknown state (ie nil) as ready".
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready && !publishNotReadyAddresses {
				continue
			}

			for _, address := range endpoint.Addresses {
				if allAddresses.Has(address) {
					continue
				}

				allAddresses.Insert(address)

				var hostname string

				switch {
				case ptr.Deref(endpoint.Hostname, "") != "":
					hostname = *endpoint.Hostname
				case k8snet.IsIPv4String(address):
					hostname = strings.ReplaceAll(address, ".", "-")
				case k8snet.IsIPv6String(address):
					hostname = strings.ReplaceAll(address, ":", "-")
				}

				record := DNSRecord{
					IP:          address,
					Ports:       mcsPorts,
					ClusterName: clusterID,
					HostName:    hostname,
				}

				clusterInfo.endpointRecords = append(clusterInfo.endpointRecords, record)

				clusterInfo.endpointRecordsByHost[hostname] = append(clusterInfo.endpointRecordsByHost[hostname], record)
			}
		}
	}

	if len(clusterInfo.endpointRecords) <= maxRecordsToLog {
		logger.Infof("Added %s records for headless EndpointSlice %q from cluster %q: %s",
			ipFamilyInfo.addrType, key, clusterID, resource.ToJSON(clusterInfo.endpointRecords))
	} else {
		logger.Infof("Added %s records for headless EndpointSlice %q from cluster %q (showing %d/%d): %s",
			ipFamilyInfo.addrType, key, clusterID, maxRecordsToLog, len(clusterInfo.endpointRecords),
			resource.ToJSON(clusterInfo.endpointRecords[:maxRecordsToLog]))
	}
}

func (i *Interface) getLocalEndpointSlices(forEPS *discovery.EndpointSlice) ([]*discovery.EndpointSlice, error) {
	epsGVR := schema.GroupVersionResource{
		Group:    discovery.SchemeGroupVersion.Group,
		Version:  discovery.SchemeGroupVersion.Version,
		Resource: "endpointslices",
	}

	list, err := i.client.Resource(epsGVR).Namespace(forEPS.Labels[constants.LabelSourceNamespace]).List(context.TODO(),
		metav1.ListOptions{
			LabelSelector: labels.Set(map[string]string{
				discovery.LabelServiceName: forEPS.Labels[mcsv1a1.LabelServiceName],
			}).String(),
		})
	if err != nil {
		return nil, errors.Wrapf(err, "error retrieving the endpointslices in namespace %s", forEPS.Labels[constants.LabelSourceNamespace])
	}

	if len(list.Items) == 0 {
		return nil, fmt.Errorf("local EndpointSlice not found for %s/%s", forEPS.Labels[constants.LabelSourceNamespace],
			forEPS.Labels[mcsv1a1.LabelServiceName])
	}

	epSlices := make([]*discovery.EndpointSlice, len(list.Items))

	for i := range list.Items {
		epSlice := &discovery.EndpointSlice{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, epSlice)
		utilruntime.Must(err)

		epSlice.Labels = forEPS.Labels
		epSlice.Annotations = forEPS.Annotations
		epSlices[i] = epSlice
	}

	return epSlices, nil
}

func (i *Interface) RemoveEndpointSlice(endpointSlice *discovery.EndpointSlice) {
	key, clusterID, ok := getKeyInfoFrom(endpointSlice)
	if !ok {
		return
	}

	logger.Infof("Remove %s EndpointSlice %q on cluster %q", endpointSlice.AddressType, key, clusterID)

	i.mutex.Lock()
	defer i.mutex.Unlock()

	svcInfo, found := i.serviceMap[key]
	if !found {
		return
	}

	ipFamilyInfo := svcInfo.getIPFamilyInfo(endpointSlice.AddressType)

	delete(ipFamilyInfo.clusters, clusterID)

	if svcInfo.canBeDeleted() {
		delete(i.serviceMap, key)
	} else if !svcInfo.isHeadless() {
		ipFamilyInfo.mergePorts()
		ipFamilyInfo.resetLoadBalancing()
	}
}

func getKeyInfoFrom(es *discovery.EndpointSlice) (string, string, bool) {
	name, ok := es.Labels[mcsv1a1.LabelServiceName]
	if !ok {
		logger.Warningf("EndpointSlice missing label %q: %#v", mcsv1a1.LabelServiceName, es.ObjectMeta)
		return "", "", false
	}

	namespace, ok := es.Labels[constants.LabelSourceNamespace]
	if !ok {
		logger.Warningf("EndpointSlice missing label %q: %#v", constants.LabelSourceNamespace, es.ObjectMeta)
		return "", "", false
	}

	clusterID, ok := es.Labels[mcsv1a1.LabelSourceCluster]
	if !ok {
		logger.Warningf("EndpointSlice missing label %q: %#v", mcsv1a1.LabelSourceCluster, es.ObjectMeta)
		return "", "", false
	}

	return keyFunc(namespace, name), clusterID, true
}

func mcsServicePortsFrom(ports []discovery.EndpointPort) []mcsv1a1.ServicePort {
	mcsPorts := make([]mcsv1a1.ServicePort, len(ports))
	for i, port := range ports {
		mcsPorts[i] = mcsv1a1.ServicePort{
			Name:        *port.Name,
			Protocol:    *port.Protocol,
			AppProtocol: port.AppProtocol,
			Port:        *port.Port,
		}
	}

	return mcsPorts
}

func isHeadless(endpointSlice *discovery.EndpointSlice) bool {
	return endpointSlice.Labels[constants.LabelIsHeadless] == strconv.FormatBool(true)
}

func shouldRetrieveLocalEndpointSlicesFor(endpointSlice *discovery.EndpointSlice) bool {
	_, found := endpointSlice.Labels[constants.LabelIsHeadless]
	if !found {
		// This is a legacy pre-0.15 EndpointSlice. We don't know if it's headless or if globalnet is enabled.
		return true
	}

	globalnetEnabled, found := endpointSlice.Annotations[constants.GlobalnetEnabled]
	if !found {
		// This is a legacy 0.15 EndpointSlice. We don't know if globalnet is enabled.
		return isHeadless(endpointSlice)
	}

	return endpointSlice.AddressType == discovery.AddressTypeIPv4 &&
		isHeadless(endpointSlice) && globalnetEnabled == strconv.FormatBool(true)
}
