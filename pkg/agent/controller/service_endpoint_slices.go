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

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/submariner-io/admiral/pkg/federate"
	"github.com/submariner-io/admiral/pkg/log"
	"github.com/submariner-io/admiral/pkg/resource"
	"github.com/submariner-io/admiral/pkg/syncer"
	"github.com/submariner-io/admiral/pkg/util"
	"github.com/submariner-io/lighthouse/pkg/constants"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	k8snet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

var AwaitStoppedTimeout = time.Second * 5

func startEndpointSliceController(localClient dynamic.Interface, restMapper meta.RESTMapper, scheme *runtime.Scheme,
	serviceImport *mcsv1a1.ServiceImport, clusterID string, globalIngressIPCache *globalIngressIPCache,
	localLHEndpointSliceLister EndpointSliceListerFn,
) (*ServiceEndpointSliceController, error) {
	serviceNamespace := serviceImport.Labels[constants.LabelSourceNamespace]
	serviceName := serviceImportSourceName(serviceImport)

	logger.V(log.DEBUG).Infof("Starting EndpointSlice controller for service %s/%s", serviceNamespace, serviceName)

	globalIngressIPGVR, _ := schema.ParseResourceArg("globalingressips.v1.submariner.io")

	controller := &ServiceEndpointSliceController{
		clusterID:                clusterID,
		serviceNamespace:         serviceNamespace,
		serviceName:              serviceName,
		serviceImportSpec:        &serviceImport.Spec,
		publishNotReadyAddresses: serviceImport.Annotations[constants.PublishNotReadyAddresses],
		stopCh:                   make(chan struct{}),
		globalIngressIPCache:     globalIngressIPCache,
		localClient:              localClient.Resource(endpointSliceGVR).Namespace(serviceNamespace),
		ingressIPClient:          localClient.Resource(*globalIngressIPGVR),
		federator:                federate.NewCreateOrUpdateFederator(localClient, restMapper, serviceNamespace, ""),
		awaitStoppedTimeout:      AwaitStoppedTimeout,
	}

	var err error

	controller.epsSyncer, err = syncer.NewResourceSyncer(&syncer.ResourceSyncerConfig{
		Name:            "K8s EndpointSlice -> LH EndpointSlice",
		SourceClient:    localClient,
		SourceNamespace: serviceNamespace,
		SourceLabelSelector: k8slabels.Set(map[string]string{
			discovery.LabelServiceName: serviceName,
		}).String(),
		RestMapper:   restMapper,
		Federator:    controller,
		ResourceType: &discovery.EndpointSlice{},
		Transform:    controller.onServiceEndpointSlice,
		Scheme:       scheme,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error creating Endpoints syncer")
	}

	if err := controller.epsSyncer.Start(controller.stopCh); err != nil {
		return nil, errors.Wrap(err, "error starting Endpoints syncer")
	}

	if controller.isHeadless() {
		controller.epsSyncer.Reconcile(func() []runtime.Object {
			list := localLHEndpointSliceLister(k8slabels.SelectorFromSet(map[string]string{
				constants.LabelSourceNamespace: serviceNamespace,
				mcsv1a1.LabelServiceName:       serviceName,
				mcsv1a1.LabelSourceCluster:     clusterID,
			}))

			retList := make([]runtime.Object, 0, len(list))

			for _, o := range list {
				eps := o.(*discovery.EndpointSlice)
				retList = append(retList, &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      eps.Labels[constants.LabelSourceName],
						Namespace: serviceNamespace,
					},
				})
			}

			return retList
		})
	}

	return controller, nil
}

func (c *ServiceEndpointSliceController) stop(ctx context.Context) error {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})

	timedCtx, cancel := context.WithTimeout(ctx, c.awaitStoppedTimeout)
	defer cancel()

	err := c.epsSyncer.AwaitStopped(timedCtx)

	return errors.Wrapf(err, "error stopping EndpointSlice syncer for %s/%s", c.serviceNamespace, c.serviceName)
}

func (c *ServiceEndpointSliceController) cleanup(ctx context.Context) (bool, error) {
	listOptions := metav1.ListOptions{
		LabelSelector: k8slabels.SelectorFromSet(map[string]string{
			discovery.LabelManagedBy:       constants.LabelValueManagedBy,
			constants.LabelSourceNamespace: c.serviceNamespace,
			mcsv1a1.LabelSourceCluster:     c.clusterID,
			mcsv1a1.LabelServiceName:       c.serviceName,
		}).String(),
	}

	list, err := c.localClient.List(ctx, listOptions)
	if err != nil {
		return false, errors.Wrapf(err, "error listing the EndpointSlices associated with service %s/%s",
			c.serviceNamespace, c.serviceName)
	}

	if len(list.Items) == 0 {
		return false, nil
	}

	err = c.localClient.DeleteCollection(ctx, metav1.DeleteOptions{}, listOptions)

	if err != nil && !apierrors.IsNotFound(err) {
		return false, errors.Wrapf(err, "error deleting the EndpointSlices associated with service %s/%s",
			c.serviceNamespace, c.serviceName)
	}

	return true, nil
}

func (c *ServiceEndpointSliceController) onServiceEndpointSlice(obj runtime.Object, _ int, op syncer.Operation) (runtime.Object, bool) {
	serviceEPS := obj.(*discovery.EndpointSlice)

	logLevel := log.DEBUG
	if op == syncer.Update {
		logLevel = log.TRACE
	}

	logger.V(logLevel).Infof("Service %s EndpointSlice \"%s/%s\" %sd",
		serviceEPS.AddressType, serviceEPS.Namespace, serviceEPS.Name, op)

	var returnEPS *discovery.EndpointSlice

	if c.isHeadless() {
		returnEPS = c.headlessEndpointSliceFrom(serviceEPS, op)
	} else {
		returnEPS = c.clusterIPEndpointSliceFrom(serviceEPS)
	}

	if returnEPS == nil {
		return nil, false
	}

	logger.V(logLevel).Infof("Returning EndpointSlice \"%s/%s\": %s", serviceEPS.Namespace, returnEPS.GenerateName,
		endpointSliceStringer{returnEPS})

	return returnEPS, false
}

func (c *ServiceEndpointSliceController) clusterIPEndpointSliceFrom(serviceEPS *discovery.EndpointSlice) *discovery.EndpointSlice {
	endpointSlice := c.newEndpointSliceFrom(serviceEPS)

	var serviceIP string

	for _, ip := range c.serviceImportSpec.IPs {
		if (serviceEPS.AddressType == discovery.AddressTypeIPv4 && k8snet.IPFamilyOfString(ip) == k8snet.IPv4) ||
			(serviceEPS.AddressType == discovery.AddressTypeIPv6 && k8snet.IPFamilyOfString(ip) == k8snet.IPv6) {
			serviceIP = ip
			break
		}
	}

	endpointSlice.Endpoints = []discovery.Endpoint{{
		Addresses: []string{serviceIP},
		Conditions: discovery.EndpointConditions{
			Ready: ptr.To(c.getReadyAddressCount(serviceEPS.AddressType) > 0),
		},
	}}

	for i := range c.serviceImportSpec.Ports {
		endpointSlice.Ports = append(endpointSlice.Ports, discovery.EndpointPort{
			Port:        &c.serviceImportSpec.Ports[i].Port,
			Name:        &c.serviceImportSpec.Ports[i].Name,
			Protocol:    &c.serviceImportSpec.Ports[i].Protocol,
			AppProtocol: c.serviceImportSpec.Ports[i].AppProtocol,
		})
	}

	return endpointSlice
}

func (c *ServiceEndpointSliceController) getReadyAddressCount(addrType discovery.AddressType) int {
	list := c.epsSyncer.ListResources()

	readyCount := 0

	for _, o := range list {
		eps := o.(*discovery.EndpointSlice)
		if eps.AddressType != addrType {
			continue
		}

		for i := range eps.Endpoints {
			// Note: we're treating nil as ready to be on the safe side as the EndpointConditions doc states
			// "In most cases consumers should interpret this unknown state (ie nil) as ready".
			if eps.Endpoints[i].Conditions.Ready == nil || *eps.Endpoints[i].Conditions.Ready {
				readyCount++
			}
		}
	}

	return readyCount
}

func (c *ServiceEndpointSliceController) headlessEndpointSliceFrom(serviceEPS *discovery.EndpointSlice, op syncer.Operation,
) *discovery.EndpointSlice {
	endpointSlice := c.newEndpointSliceFrom(serviceEPS)

	if op == syncer.Delete {
		return endpointSlice
	}

	endpointSlice.Ports = serviceEPS.Ports
	endpointSlice.Endpoints = make([]discovery.Endpoint, len(serviceEPS.Endpoints))

	for i := range serviceEPS.Endpoints {
		endpointSlice.Endpoints[i] = serviceEPS.Endpoints[i]
		endpointSlice.Endpoints[i].Addresses = c.getHeadlessEndpointAddresses(serviceEPS.Name, serviceEPS.AddressType,
			&serviceEPS.Endpoints[i])

		if len(endpointSlice.Endpoints[i].Addresses) == 0 {
			return nil
		}
	}

	return endpointSlice
}

func (c *ServiceEndpointSliceController) newEndpointSliceFrom(serviceEPS *discovery.EndpointSlice) *discovery.EndpointSlice {
	eps := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: c.serviceName + "-",
			Labels: map[string]string{
				discovery.LabelManagedBy:       constants.LabelValueManagedBy,
				constants.LabelSourceNamespace: c.serviceNamespace,
				mcsv1a1.LabelSourceCluster:     c.clusterID,
				mcsv1a1.LabelServiceName:       c.serviceName,
				constants.LabelIsHeadless:      strconv.FormatBool(c.isHeadless()),
			},
			Annotations: map[string]string{
				constants.PublishNotReadyAddresses: c.publishNotReadyAddresses,
				constants.GlobalnetEnabled:         strconv.FormatBool(c.isHeadless() && c.globalIngressIPCache != nil),
			},
		},
		AddressType: serviceEPS.AddressType,
	}

	if eps.AddressType == discovery.AddressTypeIPv6 {
		eps.GenerateName += "v6-"
	}

	for k, v := range serviceEPS.Labels {
		if !strings.Contains(k, "kubernetes.io/") {
			eps.Labels[k] = v
		}
	}

	if c.isHeadless() {
		eps.Labels[constants.LabelSourceName] = serviceEPS.Name
	}

	return eps
}

func (c *ServiceEndpointSliceController) getHeadlessEndpointAddresses(name string, addrType discovery.AddressType,
	endpoint *discovery.Endpoint,
) []string {
	if c.globalIngressIPCache == nil || addrType != discovery.AddressTypeIPv4 {
		return endpoint.Addresses
	}

	transform := func(obj *unstructured.Unstructured) (any, bool) {
		ip, _, _ := unstructured.NestedString(obj.Object, "status", "allocatedIP")
		return ip, ip != ""
	}

	requeue := func() {
		c.epsSyncer.RequeueResource(name, c.serviceNamespace)
	}

	var (
		ret    any
		found  bool
		forPod bool
	)

	if endpoint.TargetRef != nil && endpoint.TargetRef.Kind == "Pod" {
		forPod = true
		ret, found = c.globalIngressIPCache.getForPod(c.serviceNamespace, endpoint.TargetRef.Name, transform, requeue)
	} else {
		forPod = false
		ret, found = c.globalIngressIPCache.getForEndpoints(c.serviceNamespace, endpoint.Addresses[0], transform, requeue)
	}

	if !found {
		if forPod {
			logger.Infof("GlobalIP for Endpoint pod name %q is not allocated yet", endpoint.TargetRef.Name)
		} else {
			logger.Infof("GlobalIP for Endpoint IP %q is not allocated yet", endpoint.Addresses[0])
		}

		return nil
	}

	return []string{ret.(string)}
}

func (c *ServiceEndpointSliceController) isHeadless() bool {
	return c.serviceImportSpec.Type == mcsv1a1.Headless
}

func (c *ServiceEndpointSliceController) Distribute(ctx context.Context, obj runtime.Object) error {
	toDistribute := resource.MustToUnstructured(obj)
	labels := toDistribute.GetLabels()

	identifyingLabels := map[string]string{}
	if c.isHeadless() {
		identifyingLabels[constants.LabelSourceName] = labels[constants.LabelSourceName]
	} else {
		identifyingLabels[mcsv1a1.LabelServiceName] = labels[mcsv1a1.LabelServiceName]
		identifyingLabels[constants.LabelSourceNamespace] = labels[constants.LabelSourceNamespace]
		identifyingLabels[mcsv1a1.LabelSourceCluster] = labels[mcsv1a1.LabelSourceCluster]
	}

	_, _, err := util.CreateOrUpdateWithOptions[*unstructured.Unstructured](ctx, util.CreateOrUpdateOptions[*unstructured.Unstructured]{
		Client: resource.ForDynamic(c.localClient),
		Obj:    toDistribute,
		MutateOnUpdate: func(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			return util.CopyImmutableMetadata(obj, toDistribute), nil
		},
		IdentifyingLabels: identifyingLabels,
	})

	return err
}

func (c *ServiceEndpointSliceController) Delete(ctx context.Context, obj runtime.Object) error {
	if c.isHeadless() {
		list, err := c.localClient.List(ctx, metav1.ListOptions{
			LabelSelector: k8slabels.Set(map[string]string{
				constants.LabelSourceName: resource.MustToMeta(obj).GetLabels()[constants.LabelSourceName],
			}).String(),
		})
		if err != nil {
			return errors.Wrap(err, "error listing EndpointSlice resources for delete")
		}

		if len(list.Items) == 0 {
			logger.V(log.DEBUG).Infof("Existing EndpointSlice not found for service EPS %q",
				resource.MustToMeta(obj).GetLabels()[constants.LabelSourceName])
			return nil
		}

		return c.localClient.Delete(ctx, list.Items[0].GetName(), metav1.DeleteOptions{}) //nolint:wrapcheck // No need to wrap here
	}

	// For a non-headless service, we never delete the single exported EPS - we update its endpoint condition based on
	// the backend service EPS's as they are created/updated/deleted.
	return c.Distribute(ctx, obj)
}

type endpointSliceStringer struct {
	*discovery.EndpointSlice
}

func (s endpointSliceStringer) String() string {
	labels := resource.ToJSON(&s.Labels)
	ports := resource.ToJSON(&s.Ports)
	endpoints := resource.ToJSON(&s.Endpoints)

	return fmt.Sprintf("\nlabels: %s\naddressType: %s\nendpoints: %s\nports: %s", labels, s.AddressType, endpoints, ports)
}
