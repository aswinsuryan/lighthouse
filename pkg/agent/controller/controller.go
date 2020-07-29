package controller

import (
	"context"
	"fmt"

	"k8s.io/client-go/util/retry"

	"github.com/submariner-io/admiral/pkg/syncer/broker"

	"k8s.io/apimachinery/pkg/selection"

	"k8s.io/client-go/kubernetes"

	lighthousev2a1 "github.com/submariner-io/lighthouse/pkg/apis/lighthouse.submariner.io/v2alpha1"
	lighthouseClientset "github.com/submariner-io/lighthouse/pkg/client/clientset/versioned"
	lighthouseInformers "github.com/submariner-io/lighthouse/pkg/client/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

type ServiceImportController struct {
	// Indirection hook for unit tests to supply fake client sets
	kubeClientSet    kubernetes.Interface
	lighthouseClient lighthouseClientset.Interface
	kubeConfig       *rest.Config
	serviceInformer  cache.SharedIndexInformer
	endpointInformer map[string]informers.SharedInformerFactory
	queue            workqueue.RateLimitingInterface
	svcSyncer        *broker.Syncer
	clusterID        string
	stopCh           chan struct{}
}

func NewController(spec *AgentSpecification, cfg *rest.Config) (*ServiceImportController, error) {

	clientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("Error building clientset: %s", err.Error())
	}

	lighthouseClient, err := lighthouseClientset.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("Error building lighthouseClient %s", err.Error())
	}

	serviceImportController := ServiceImportController{
		queue:            workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		kubeClientSet:    clientSet,
		lighthouseClient: lighthouseClient,
		stopCh:           make(chan struct{}),
	}
	return &serviceImportController, nil
}

func (c *ServiceImportController) Start(kubeConfig *rest.Config) error {
	klog.Infof("Starting ServiceImport Controller")

	c.kubeConfig = kubeConfig

	informerFactory := lighthouseInformers.NewSharedInformerFactoryWithOptions(c.lighthouseClient, 0,
		lighthouseInformers.WithNamespace(metav1.NamespaceAll))

	c.serviceInformer = informerFactory.Lighthouse().V2alpha1().ServiceImports().Informer()
	c.serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			klog.V(2).Infof("ServiceImport %q added", key)
			if err == nil {
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(obj interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			klog.V(2).Infof("ServiceImport %q updated", key)
			if err == nil {
				c.queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			klog.V(2).Infof("ServiceImport %q deleted", key)
			if err == nil {
				c.serviceImportDeleted(obj, key)
			}
		},
	})

	go c.serviceInformer.Run(c.stopCh)
	go c.runWorker()

	return nil
}

func (c *ServiceImportController) Stop() {
	close(c.stopCh)
	c.queue.ShutDown()

	klog.Infof("ServiceImport Controller stopped")
}

func (c *ServiceImportController) runWorker() {
	for {
		keyObj, shutdown := c.queue.Get()
		if shutdown {
			klog.Infof("Lighthouse watcher for ServiceImports stopped")
			return
		}

		key := keyObj.(string)
		func() {
			defer c.queue.Done(key)
			obj, exists, err := c.serviceInformer.GetIndexer().GetByKey(key)

			if err != nil {
				klog.Errorf("Error retrieving service with key %q from the cache: %v", key, err)
				// requeue the item to work on later
				c.queue.AddRateLimited(key)
				return
			}

			if exists {
				c.serviceImportCreatedOrUpdated(obj, key)
			}

			c.queue.Forget(key)
		}()
	}
}
func (c *ServiceImportController) handleAddedOrUpdated(obj interface{}, key string) {
	switch v := obj.(type) {
	default:
		klog.Infof("unexpected type %T", v)
	case *lighthousev2a1.ServiceImport:
		c.serviceImportCreatedOrUpdated(obj, key)
	case *corev1.Endpoints:
		c.endPointCreatedOrUpdated(obj, key)
	}
}
func (c *ServiceImportController) serviceImportCreatedOrUpdated(obj interface{}, key string) {
	klog.V(2).Infof("In serviceImportCreatedOrUpdated for key %q, service: %#v, ", key, obj)

	serviceImportCreated := obj.(*lighthousev2a1.ServiceImport)
	if serviceImportCreated.Spec.Type == lighthousev2a1.Headless {
		matchLabels := serviceImportCreated.GetObjectMeta().GetLabels()
		labelSelector := labels.NewSelector()
		endPointAppLabel, _ := labels.NewRequirement("app", selection.DoesNotExist, []string{matchLabels["app"]})
		labelSelector = labelSelector.Add(*endPointAppLabel)
		clientSet := c.kubeClientSet
		c.endpointInformer[key] = informers.NewSharedInformerFactoryWithOptions(clientSet, 0,
			informers.WithTweakListOptions(func(options *metav1.ListOptions) {
				options.LabelSelector = labelSelector.String()
			}))
		c.endpointInformer[key].Core().V1().Endpoints().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				klog.V(2).Infof("Endpoint %q added", key)
				if err == nil {
					c.queue.Add(key)
				}
			},
			UpdateFunc: func(obj interface{}, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				klog.V(2).Infof("Endpoint %q updated", key)
				if err == nil {
					c.queue.Add(key)
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				klog.V(2).Infof("Endpoint %q deleted", key)
				if err == nil {
					c.serviceImportDeleted(obj, key)
				}
			},
		})
	}
}

func (c *ServiceImportController) serviceImportDeleted(obj interface{}, key string) {
	var mcs *lighthousev2a1.ServiceImport
	var ok bool
	if mcs, ok = obj.(*lighthousev2a1.ServiceImport); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			//TODO Remove mcs
			klog.Errorf("Failed to get deleted serviceimport object for %s", key, mcs)
			return
		}
		mcs, ok = tombstone.Obj.(*lighthousev2a1.ServiceImport)
		if !ok {
			klog.Errorf("Failed to convert deleted tombstone object %v  to serviceimport", tombstone.Obj)
			return
		}
	}
}

func (c *ServiceImportController) endPointCreatedOrUpdated(obj interface{}, key string) error {
	klog.V(2).Infof("In serviceImportCreatedOrUpdated for key %q, service: %#v, ", key, obj)
	endPoints := obj.(*corev1.Endpoints)
	endpointSliceName := endPoints.Name + "-" + c.clusterID
	newEndPointSlice := c.endpointSliceFromEndpoints(endPoints)
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		currentEndpointSice, err := c.kubeClientSet.DiscoveryV1beta1().EndpointSlices(endPoints.Namespace).
			Get(context.TODO(), endpointSliceName, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("Retriving EndpointSlice %s from NameSpace %s failed", endpointSliceName, endPoints.Namespace)
			return err
		}
		if currentEndpointSice != nil {
			//TODO Check if the Current and new is Equal and if so return.
		}
		_, err = c.kubeClientSet.DiscoveryV1beta1().EndpointSlices(endPoints.Namespace).Create(context.TODO(), newEndPointSlice, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("Creating EndpointSlice %s from NameSpace %s failed", endpointSliceName, endPoints.Namespace)
			return err
		}
		return nil
	})
	return retryErr
}

func (c *ServiceImportController) endpointSliceFromEndpoints(endpoints *corev1.Endpoints) *discovery.EndpointSlice {
	endpointSlice := &discovery.EndpointSlice{}
	endpointSlice.Name = endpoints.Name + "-" + c.clusterID
	endpointSlice.Labels = map[string]string{discovery.LabelServiceName: endpoints.Name}

	endpointSlice.AddressType = discovery.AddressTypeIPv4

	if len(endpoints.Subsets) > 0 {
		subset := endpoints.Subsets[0]
		for i := range subset.Ports {
			endpointSlice.Ports = append(endpointSlice.Ports, discovery.EndpointPort{
				Port:     &subset.Ports[i].Port,
				Name:     &subset.Ports[i].Name,
				Protocol: &subset.Ports[i].Protocol,
			})
		}

		if allAddressesIPv6(append(subset.Addresses, subset.NotReadyAddresses...)) {
			endpointSlice.AddressType = discovery.AddressTypeIPv6
		}

		endpointSlice.Endpoints = append(endpointSlice.Endpoints, getEndpointsFromAddresses(subset.Addresses, endpointSlice.AddressType, true)...)
		endpointSlice.Endpoints = append(endpointSlice.Endpoints, getEndpointsFromAddresses(subset.NotReadyAddresses, endpointSlice.AddressType, false)...)
	}

	return endpointSlice
}

// getEndpointsFromAddresses returns a list of Endpoints from addresses that
// match the provided address type.
func getEndpointsFromAddresses(addresses []corev1.EndpointAddress, addressType discovery.AddressType, ready bool) []discovery.Endpoint {
	endpoints := []discovery.Endpoint{}
	isIPv6AddressType := addressType == discovery.AddressTypeIPv6

	for _, address := range addresses {
		if utilnet.IsIPv6String(address.IP) == isIPv6AddressType {
			endpoints = append(endpoints, endpointFromAddress(address, ready))
		}
	}

	return endpoints
}

// endpointFromAddress generates an Endpoint from an EndpointAddress resource.
func endpointFromAddress(address corev1.EndpointAddress, ready bool) discovery.Endpoint {
	topology := map[string]string{}
	if address.NodeName != nil {
		topology["kubernetes.io/hostname"] = *address.NodeName
	}

	return discovery.Endpoint{
		Addresses:  []string{address.IP},
		Conditions: discovery.EndpointConditions{Ready: &ready},
		TargetRef:  address.TargetRef,
		Topology:   topology,
	}
}

// allAddressesIPv6 returns true if all provided addresses are IPv6.
func allAddressesIPv6(addresses []corev1.EndpointAddress) bool {
	if len(addresses) == 0 {
		return false
	}

	for _, address := range addresses {
		if !utilnet.IsIPv6String(address.IP) {
			return false
		}
	}

	return true
}
