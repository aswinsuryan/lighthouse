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

	"github.com/pkg/errors"
	"github.com/submariner-io/lighthouse/pkg/constants"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

var (
	serviceImportGVR = schema.GroupVersionResource{
		Group:    mcsv1a1.GroupName,
		Version:  mcsv1a1.GroupVersion.Version,
		Resource: "serviceimports",
	}

	endpointSliceGVR = schema.GroupVersionResource{
		Group:    discovery.GroupName,
		Version:  discovery.SchemeGroupVersion.Version,
		Resource: "endpointslices",
	}
)

func (a *Controller) Cleanup(ctx context.Context) error {
	// Delete all ServiceImports from the local cluster skipping those in the broker namespace if the broker is on the
	// local cluster.
	siClient := a.serviceImportController.localClient.Resource(serviceImportGVR)

	list, err := listResources(ctx, siClient, metav1.NamespaceAll,
		&metav1.ListOptions{
			FieldSelector: fields.OneTermNotEqualSelector("metadata.namespace",
				a.serviceImportController.serviceImportAggregator.brokerNamespace).String(),
		})
	if err != nil {
		return errors.Wrap(err, "error listing local ServiceImports")
	}

	for i := range list {
		_, ok := list[i].GetLabels()[mcsv1a1.LabelServiceName]
		if ok {
			err = a.serviceImportController.Delete(ctx, &list[i])
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}

		err = siClient.Namespace(list[i].GetNamespace()).Delete(ctx, list[i].GetName(), metav1.DeleteOptions{})

		if err != nil && !apierrors.IsNotFound(err) {
			return err //nolint:wrapcheck // Let the caller wrap
		}
	}

	// Delete all EndpointSlices from the local cluster skipping those in the broker namespace if the broker is on the
	// local cluster.
	err = deleteResources(ctx, a.endpointSliceController.syncer.GetLocalClient().Resource(endpointSliceGVR), metav1.NamespaceAll,
		&metav1.ListOptions{
			FieldSelector: fields.OneTermNotEqualSelector("metadata.namespace",
				a.serviceImportController.serviceImportAggregator.brokerNamespace).String(),
			LabelSelector: labels.Set(map[string]string{discovery.LabelManagedBy: constants.LabelValueManagedBy}).String(),
		})
	if err != nil {
		return errors.Wrap(err, "error deleting local EndpointSlices")
	}

	// Delete all local EndpointSlices from the broker.
	err = deleteResources(ctx, a.endpointSliceController.syncer.GetBrokerClient().Resource(endpointSliceGVR),
		a.endpointSliceController.syncer.GetBrokerNamespace(),
		&metav1.ListOptions{
			LabelSelector: labels.Set(map[string]string{mcsv1a1.LabelSourceCluster: a.clusterID}).String(),
		})

	return errors.Wrap(err, "error deleting remote EndpointSlices")
}

func deleteResources(ctx context.Context, client dynamic.NamespaceableResourceInterface, ns string, options *metav1.ListOptions) error {
	list, err := listResources(ctx, client, ns, options)
	if err != nil {
		return err
	}

	for i := range list {
		err = client.Namespace(list[i].GetNamespace()).Delete(ctx, list[i].GetName(), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err //nolint:wrapcheck // Let the caller wrap
		}
	}

	return nil
}

func listResources(ctx context.Context, client dynamic.NamespaceableResourceInterface, ns string,
	options *metav1.ListOptions,
) ([]unstructured.Unstructured, error) {
	list, err := client.Namespace(ns).List(ctx, *options)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err //nolint:wrapcheck // Let the caller wrap
	}

	return list.Items, nil
}
