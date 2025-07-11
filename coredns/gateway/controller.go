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

package gateway

import (
	"context"
	"maps"
	"os"
	"sync/atomic"

	"github.com/pkg/errors"
	"github.com/submariner-io/admiral/pkg/log"
	"github.com/submariner-io/admiral/pkg/resource"
	"github.com/submariner-io/admiral/pkg/workqueue"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	k8snet "k8s.io/utils/net"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var logger = log.Logger{Logger: logf.Log.WithName("Gateway")}

type clusterStatusMapType map[k8snet.IPFamily]map[string]bool

type Controller struct {
	informer         cache.Controller
	store            cache.Store
	queue            workqueue.Interface
	stopCh           chan struct{}
	clusterStatusMap atomic.Value
	localClusterID   atomic.Value
	gatewayAvailable bool
}

func NewController() *Controller {
	controller := &Controller{
		queue:            workqueue.New("Gateway Controller"),
		stopCh:           make(chan struct{}),
		gatewayAvailable: true,
	}

	controller.clusterStatusMap.Store(clusterStatusMapType{
		k8snet.IPv4: {},
		k8snet.IPv6: {},
	})

	localClusterID := os.Getenv("SUBMARINER_CLUSTERID")

	logger.Infof("Setting localClusterID from env: %q", localClusterID)
	controller.localClusterID.Store(localClusterID)

	return controller
}

func (c *Controller) Start(client dynamic.Interface) error {
	gwClientset, err := c.getCheckedClientset(client)
	if apierrors.IsNotFound(err) {
		logger.Infof("Connectivity component is not installed, disabling Gateway status controller")

		c.gatewayAvailable = false

		return nil
	}

	if err != nil {
		return err
	}

	logger.Infof("Starting Gateway status Controller")

	c.store, c.informer = cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return gwClientset.List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return gwClientset.Watch(context.TODO(), options)
			},
		},
		ObjectType: &unstructured.Unstructured{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: c.queue.Enqueue,
			UpdateFunc: func(_ interface{}, newObj interface{}) {
				c.queue.Enqueue(newObj)
			},
			DeleteFunc: func(obj interface{}) {
				key, _ := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				logger.V(log.DEBUG).Infof("GatewayStatus %q deleted", key)
			},
		},
	})

	go c.informer.Run(c.stopCh)

	if ok := cache.WaitForCacheSync(c.stopCh, c.informer.HasSynced); !ok {
		return errors.New("failed to wait for informer cache to sync")
	}

	go c.queue.Run(c.processNextGateway)

	return nil
}

func (c *Controller) Stop() {
	close(c.stopCh)
	c.queue.ShutDown()
	logger.Infof("Gateway status Controller stopped")
}

func (c *Controller) IsConnected(clusterID string, ipFamily k8snet.IPFamily) bool {
	return !c.gatewayAvailable || c.getClusterStatusMap()[ipFamily][clusterID]
}

func (c *Controller) GetLocalClusterID() string {
	return c.localClusterID.Load().(string)
}

func (c *Controller) processNextGateway(key, _, _ string) (bool, error) {
	obj, exists, err := c.store.GetByKey(key)
	if err != nil {
		// requeue the item to work on later.
		return true, errors.Wrapf(err, "error retrieving Gateway with key %q from the cache", key)
	}

	if exists {
		c.gatewayCreatedOrUpdated(obj.(*unstructured.Unstructured))
	}

	return false, nil
}

func (c *Controller) gatewayCreatedOrUpdated(obj *unstructured.Unstructured) {
	connections, localClusterID, ok := getGatewayStatus(obj)
	if !ok {
		return
	}

	// Updating
	c.updateLocalClusterIDIfNeeded(localClusterID)

	c.updateClusterStatusMap(connections)
}

func (c *Controller) updateClusterStatusMap(connections []interface{}) {
	var newMap clusterStatusMapType

	currentMap := c.getClusterStatusMap()

	for _, connection := range connections {
		connectionMap := connection.(map[string]interface{})

		status, found, err := unstructured.NestedString(connectionMap, "status")
		if err != nil || !found {
			logger.Errorf(nil, "status field not found in %s", resource.ToJSON(connectionMap))
		}

		clusterID, found, err := unstructured.NestedString(connectionMap, "endpoint", "cluster_id")
		if !found || err != nil {
			logger.Errorf(nil, "cluster_id field not found in %s", resource.ToJSON(connectionMap))
			continue
		}

		usingIP, found, err := unstructured.NestedString(connectionMap, "usingIP")
		if !found || err != nil {
			logger.Errorf(nil, "usingIP field not found in %s", resource.ToJSON(connectionMap))
			continue
		}

		ipFamily := k8snet.IPFamilyOfString(usingIP)

		if status == "connected" {
			_, found := currentMap[ipFamily][clusterID]
			if !found {
				if newMap == nil {
					newMap = copyClusterStatusMap(currentMap)
				}

				newMap[ipFamily][clusterID] = true
			}
		} else {
			_, found = currentMap[ipFamily][clusterID]
			if found {
				if newMap == nil {
					newMap = copyClusterStatusMap(currentMap)
				}

				delete(newMap[ipFamily], clusterID)
			}
		}
	}

	if newMap != nil {
		logger.Infof("Updating the gateway status %s", resource.ToJSON(newMap))
		c.clusterStatusMap.Store(newMap)
	}
}

func (c *Controller) updateLocalClusterIDIfNeeded(clusterID string) {
	updateNeeded := clusterID != "" && clusterID != c.GetLocalClusterID()
	if updateNeeded {
		logger.Infof("Updating the gateway localClusterID %q ", clusterID)
		c.localClusterID.Store(clusterID)
	}
}

func getGatewayStatus(obj *unstructured.Unstructured) ([]interface{}, string, bool) {
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if !found || err != nil {
		logger.Errorf(err, "status field not found in %#v, err was", obj)
		return nil, "", false
	}

	localClusterID, found, err := unstructured.NestedString(status, "localEndpoint", "cluster_id")

	var connections []interface{}

	if !found || err != nil {
		logger.Errorf(err, "localEndpoint->cluster_id not found in %#v, err was", status)

		localClusterID = ""
	} else {
		// Add connection entries for IPv4 and IPv6, distinguished by the "usingIP" field. Note, we use any address
		// as only IP family is relevant.
		connections = append(connections, map[string]interface{}{
			"status":  "connected",
			"usingIP": "127.0.0.0",
			"endpoint": map[string]interface{}{
				"cluster_id": localClusterID,
			},
		}, map[string]interface{}{
			"status":  "connected",
			"usingIP": "::1",
			"endpoint": map[string]interface{}{
				"cluster_id": localClusterID,
			},
		})
	}

	haStatus, found, err := unstructured.NestedString(status, "haStatus")

	if !found || err != nil {
		logger.Errorf(err, "haStatus field not found in %#v, err was", status)
		return connections, localClusterID, true
	}

	if haStatus == "active" {
		rconns, _, err := unstructured.NestedSlice(status, "connections")
		if err != nil {
			logger.Errorf(err, "connections field not found in %#v, err was", status)
			return connections, localClusterID, false
		}

		connections = append(connections, rconns...)
	}

	return connections, localClusterID, true
}

func (c *Controller) getClusterStatusMap() clusterStatusMapType {
	return c.clusterStatusMap.Load().(clusterStatusMapType)
}

func (c *Controller) getCheckedClientset(client dynamic.Interface) (dynamic.ResourceInterface, error) {
	// First check if the Submariner resource is present.
	gvr, _ := schema.ParseResourceArg("submariners.v1alpha1.submariner.io")
	list, err := client.Resource(*gvr).Namespace(v1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})

	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) || (err == nil && len(list.Items) == 0) {
		return nil, apierrors.NewNotFound(gvr.GroupResource(), "")
	}

	if err != nil {
		return nil, errors.Wrap(err, "error listing Submariner resources")
	}

	gvr, _ = schema.ParseResourceArg("gateways.v1.submariner.io")
	gwClient := client.Resource(*gvr).Namespace(v1.NamespaceAll)
	_, err = gwClient.List(context.TODO(), metav1.ListOptions{})

	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, apierrors.NewNotFound(gvr.GroupResource(), "")
	}

	if err != nil {
		return nil, errors.Wrap(err, "error listing Gateway resources")
	}

	return gwClient, nil
}

func copyClusterStatusMap(src clusterStatusMapType) clusterStatusMapType {
	return clusterStatusMapType{
		k8snet.IPv4: maps.Clone(src[k8snet.IPv4]),
		k8snet.IPv6: maps.Clone(src[k8snet.IPv6]),
	}
}
