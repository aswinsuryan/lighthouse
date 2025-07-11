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

package controller_test

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/submariner-io/admiral/pkg/fake"
	"github.com/submariner-io/admiral/pkg/ipam"
	"github.com/submariner-io/admiral/pkg/log/kzerolog"
	"github.com/submariner-io/admiral/pkg/resource"
	"github.com/submariner-io/admiral/pkg/syncer/broker"
	"github.com/submariner-io/admiral/pkg/syncer/test"
	testutil "github.com/submariner-io/admiral/pkg/test"
	"github.com/submariner-io/lighthouse/pkg/agent/controller"
	"github.com/submariner-io/lighthouse/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	k8snet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

const (
	clusterID1       = "east"
	clusterID2       = "west"
	serviceName      = "nginx"
	serviceNamespace = "service-ns"
	ipV4ServiceIP1   = "10.253.9.1"
	ipV4ServiceIP2   = "10.253.10.1"
	globalIP1        = "242.254.1.1"
	globalIP2        = "242.254.1.2"
	globalIP3        = "242.254.1.3"
	epIP1            = "192.168.5.1"
	epIP2            = "192.168.5.2"
	epIP3            = "10.253.6.1"
	epIP4            = "10.253.6.2"
)

var (
	nodeName = "my-node"
	hostName = "my-host"
	host1    = "host1"
	host2    = "host2"

	port1 = mcsv1a1.ServicePort{
		Name:     "http",
		Protocol: corev1.ProtocolTCP,
		Port:     8080,
	}

	port2 = mcsv1a1.ServicePort{
		Name:     "https",
		Protocol: corev1.ProtocolTCP,
		Port:     8443,
	}

	port3 = mcsv1a1.ServicePort{
		Name:        "POP3",
		Protocol:    corev1.ProtocolUDP,
		Port:        110,
		AppProtocol: ptr.To("smtp"),
	}
)

func init() {
	// set logging verbosity of agent in unit test to DEBUG
	flags := flag.NewFlagSet("kzerolog", flag.ExitOnError)
	kzerolog.AddFlags(flags)
	//nolint:errcheck // Ignore errors; CommandLine is set for ExitOnError.
	flags.Parse([]string{"-v=2"})
	kzerolog.InitK8sLogging()

	err := mcsv1a1.Install(scheme.Scheme)
	if err != nil {
		panic(err)
	}
}

func TestController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Agent Controller Suite")
}

type cluster struct {
	agentSpec                  controller.AgentSpecification
	localDynClient             dynamic.Interface
	localDynClientFake         *k8stesting.Fake
	localServiceExportClient   dynamic.ResourceInterface
	localServiceImportClient   dynamic.NamespaceableResourceInterface
	localIngressIPClient       dynamic.ResourceInterface
	localEndpointSliceClient   dynamic.ResourceInterface
	localServiceImportReactor  *fake.FailingReactor
	agentController            *controller.Controller
	service                    *corev1.Service
	expectedClusterIPEndpoints []discovery.Endpoint
	serviceExport              *mcsv1a1.ServiceExport
	serviceEndpointSlices      []discovery.EndpointSlice
	clusterID                  string
	headlessEndpointAddresses  [][]discovery.Endpoint
}

type testDriver struct {
	cluster1                        cluster
	cluster2                        cluster
	brokerServiceImportClient       dynamic.NamespaceableResourceInterface
	brokerEndpointSliceClient       dynamic.ResourceInterface
	brokerEndpointSliceReactor      *fake.FailingReactor
	stopCh                          chan struct{}
	syncerConfig                    *broker.SyncerConfig
	doStart                         bool
	useClusterSetIP                 bool
	ipPool                          *ipam.IPPool
	brokerServiceImportReactor      *fake.FailingReactor
	aggregatedServicePorts          []mcsv1a1.ServicePort
	aggregatedSessionAffinity       corev1.ServiceAffinity
	aggregatedSessionAffinityConfig *corev1.SessionAffinityConfig
}

func newTestDiver() *testDriver {
	syncerScheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(syncerScheme)).To(Succeed())
	Expect(discovery.AddToScheme(syncerScheme)).To(Succeed())
	Expect(mcsv1a1.Install(syncerScheme)).To(Succeed())

	syncerScheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "submariner.io",
		Version: "v1",
		Kind:    "GlobalIngressIPList",
	}, &unstructured.UnstructuredList{})

	brokerClient := dynamicfake.NewSimpleDynamicClient(syncerScheme)
	fake.AddBasicReactors(&brokerClient.Fake)

	t := &testDriver{
		aggregatedServicePorts:    []mcsv1a1.ServicePort{port1, port2},
		aggregatedSessionAffinity: corev1.ServiceAffinityNone,
		cluster1: cluster{
			clusterID: clusterID1,
			agentSpec: controller.AgentSpecification{
				ClusterID:        clusterID1,
				Namespace:        test.LocalNamespace,
				GlobalnetEnabled: false,
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: serviceNamespace,
					Labels: map[string]string{
						"service-label1": "value1",
						"service-label2": "value2",
					},
				},
				Spec: corev1.ServiceSpec{
					ClusterIP:       ipV4ServiceIP1,
					ClusterIPs:      []string{ipV4ServiceIP1},
					IPFamilies:      []corev1.IPFamily{corev1.IPv4Protocol},
					Selector:        map[string]string{"app": "test"},
					Ports:           []corev1.ServicePort{toServicePort(port1), toServicePort(port2)},
					SessionAffinity: corev1.ServiceAffinityNone,
				},
			},
			serviceExport: &mcsv1a1.ServiceExport{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: serviceNamespace,
				},
			},
			serviceEndpointSlices: []discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("%s-%s1", serviceName, clusterID1),
						Labels: map[string]string{
							discovery.LabelServiceName:      serviceName,
							"kubernetes.io/cluster-service": "true",
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses:  []string{epIP1},
							Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
							Hostname:   ptr.To(hostName),
							TargetRef: &corev1.ObjectReference{
								Kind: "Pod",
								Name: "one",
							},
						},
						{
							Addresses:  []string{epIP2},
							Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
							NodeName:   ptr.To(nodeName),
							TargetRef: &corev1.ObjectReference{
								Kind: "Pod",
								Name: "two",
							},
						},
						{
							Addresses:  []string{epIP3},
							Conditions: discovery.EndpointConditions{Ready: ptr.To(false)},
							TargetRef: &corev1.ObjectReference{
								Kind: "Pod",
								Name: "not-ready",
							},
						},
					},
					Ports: []discovery.EndpointPort{
						{
							Name:     ptr.To(port1.Name),
							Protocol: &port1.Protocol,
							Port:     &port1.Port,
						},
						{
							Name:     ptr.To(port2.Name),
							Protocol: &port2.Protocol,
							Port:     &port2.Port,
						},
					},
				},
			},
		},
		cluster2: cluster{
			clusterID: clusterID2,
			agentSpec: controller.AgentSpecification{
				ClusterID:        clusterID2,
				Namespace:        test.LocalNamespace,
				GlobalnetEnabled: false,
			},
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: serviceNamespace,
				},
				Spec: corev1.ServiceSpec{
					ClusterIP:       ipV4ServiceIP2,
					ClusterIPs:      []string{ipV4ServiceIP2},
					IPFamilies:      []corev1.IPFamily{corev1.IPv4Protocol},
					Selector:        map[string]string{"app": "test"},
					Ports:           []corev1.ServicePort{toServicePort(port1), toServicePort(port2)},
					SessionAffinity: corev1.ServiceAffinityNone,
				},
			},
			serviceExport: &mcsv1a1.ServiceExport{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: serviceNamespace,
				},
			},
			serviceEndpointSlices: []discovery.EndpointSlice{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   fmt.Sprintf("%s-%s1", serviceName, clusterID2),
						Labels: map[string]string{discovery.LabelServiceName: serviceName},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses:  []string{"192.168.5.3"},
							Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
							Hostname:   &hostName,
						},
					},
				},
			},
		},
		syncerConfig: &broker.SyncerConfig{
			BrokerNamespace: test.RemoteNamespace,
			RestMapper: test.GetRESTMapperFor(&mcsv1a1.ServiceExport{}, &mcsv1a1.ServiceImport{}, &corev1.Service{},
				&corev1.Endpoints{}, &corev1.Namespace{}, &discovery.EndpointSlice{}, controller.GetGlobalIngressIPObj()),
			BrokerClient: brokerClient,
			Scheme:       syncerScheme,
		},
		stopCh:  make(chan struct{}),
		doStart: true,
	}

	var err error

	t.ipPool, err = ipam.NewIPPool("243.10.1.0/24", nil)
	Expect(err).To(Succeed())

	t.brokerServiceImportReactor = fake.NewFailingReactorForResource(&brokerClient.Fake, "serviceimports")
	t.brokerEndpointSliceReactor = fake.NewFailingReactorForResource(&brokerClient.Fake, "endpointslices")

	t.cluster1.headlessEndpointAddresses = [][]discovery.Endpoint{t.cluster1.serviceEndpointSlices[0].Endpoints}

	t.cluster2.headlessEndpointAddresses = [][]discovery.Endpoint{t.cluster2.serviceEndpointSlices[0].Endpoints}

	t.brokerServiceImportClient = t.syncerConfig.BrokerClient.Resource(*test.GetGroupVersionResourceFor(t.syncerConfig.RestMapper,
		&mcsv1a1.ServiceImport{}))

	t.brokerEndpointSliceClient = t.syncerConfig.BrokerClient.Resource(*test.GetGroupVersionResourceFor(t.syncerConfig.RestMapper,
		&discovery.EndpointSlice{})).Namespace(test.RemoteNamespace)

	t.cluster1.init(t.syncerConfig, nil, nil)
	t.cluster2.init(t.syncerConfig, nil, nil)

	return t
}

func (t *testDriver) justBeforeEach() {
	t.cluster1.start(t, *t.syncerConfig)
	t.cluster2.start(t, *t.syncerConfig)
}

func (t *testDriver) afterEach() {
	close(t.stopCh)
}

func (c *cluster) init(syncerConfig *broker.SyncerConfig, dynClient dynamic.Interface, dynClientFake *k8stesting.Fake) {
	c.expectedClusterIPEndpoints = nil

	if dynClient == nil {
		fakeDynClient := dynamicfake.NewSimpleDynamicClient(syncerConfig.Scheme)
		c.localDynClient = fakeDynClient
		c.localDynClientFake = &fakeDynClient.Fake
		fake.AddBasicReactors(c.localDynClientFake)
	} else {
		c.localDynClient = dynClient
		c.localDynClientFake = dynClientFake
	}

	c.localServiceImportReactor = fake.NewFailingReactorForResource(c.localDynClientFake, "serviceimports")

	c.localServiceExportClient = c.localDynClient.Resource(*test.GetGroupVersionResourceFor(syncerConfig.RestMapper,
		&mcsv1a1.ServiceExport{})).Namespace(serviceNamespace)

	c.localServiceImportClient = c.localDynClient.Resource(*test.GetGroupVersionResourceFor(syncerConfig.RestMapper,
		&mcsv1a1.ServiceImport{}))

	c.localEndpointSliceClient = c.localDynClient.Resource(*test.GetGroupVersionResourceFor(syncerConfig.RestMapper,
		&discovery.EndpointSlice{})).Namespace(serviceNamespace)

	c.localIngressIPClient = c.localDynClient.Resource(*test.GetGroupVersionResourceFor(syncerConfig.RestMapper,
		controller.GetGlobalIngressIPObj())).Namespace(serviceNamespace)

	// Add a K8s EPS for some other service to ensure it doesn't interfere with anything.
	_, err := endpointSliceClientFor(c.localDynClient, c.service.Namespace).Create(context.TODO(),
		resource.MustToUnstructured(&discovery.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "some-other-service-eps",
				Labels: map[string]string{discovery.LabelServiceName: "some-other-service"},
			},
		}), metav1.CreateOptions{})
	Expect(err).To(Succeed())
}

//nolint:gocritic // (hugeParam) This function modifies syncerConf so we don't want to pass by pointer.
func (c *cluster) start(t *testDriver, syncerConfig broker.SyncerConfig) {
	for i := range c.serviceEndpointSlices {
		for k, v := range c.service.Labels {
			c.serviceEndpointSlices[i].Labels[k] = v
		}
	}

	for _, ip := range c.service.Spec.ClusterIPs {
		c.expectedClusterIPEndpoints = append(c.expectedClusterIPEndpoints, discovery.Endpoint{
			Addresses:  []string{ip},
			Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
		})
	}

	syncerConfig.LocalClient = c.localDynClient
	bigint, err := rand.Int(rand.Reader, big.NewInt(1000000))
	Expect(err).To(Succeed())

	serviceImportCounterName := "submariner_service_import" + bigint.String()

	bigint, err = rand.Int(rand.Reader, big.NewInt(1000000))
	Expect(err).To(Succeed())

	serviceExportCounterName := "submariner_service_export" + bigint.String()

	c.agentController, err = controller.New(&c.agentSpec, syncerConfig,
		controller.AgentConfig{
			ServiceImportCounterName: serviceImportCounterName,
			ServiceExportCounterName: serviceExportCounterName,
			IPPool:                   t.ipPool,
		})

	Expect(err).To(Succeed())

	if t.doStart {
		Expect(c.agentController.Start(t.stopCh)).To(Succeed())
	}
}

func (c *cluster) createService() {
	test.CreateResource(c.dynamicServiceClientFor().Namespace(c.service.Namespace), c.service)
}

func (c *cluster) updateService() {
	test.UpdateResource(c.dynamicServiceClientFor().Namespace(c.service.Namespace), c.service)
}

func (c *cluster) deleteService() {
	Expect(c.dynamicServiceClientFor().Namespace(c.service.Namespace).Delete(context.TODO(), c.service.Name,
		metav1.DeleteOptions{})).To(Succeed())
}

func (c *cluster) createServiceExport() {
	test.CreateResource(c.localServiceExportClient, c.serviceExport)
}

func (c *cluster) deleteServiceExport() {
	Expect(c.localServiceExportClient.Delete(context.TODO(), c.serviceExport.GetName(), metav1.DeleteOptions{})).To(Succeed())
}

func (c *cluster) createServiceEndpointSlices() {
	client := endpointSliceClientFor(c.localDynClient, c.service.Namespace)

	for i := range c.serviceEndpointSlices {
		_, err := client.Create(context.TODO(), resource.MustToUnstructured(&c.serviceEndpointSlices[i]), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			continue
		}

		Expect(err).To(Succeed())
	}
}

func (c *cluster) updateServiceEndpointSlices() {
	client := endpointSliceClientFor(c.localDynClient, c.service.Namespace)

	for i := range c.serviceEndpointSlices {
		test.UpdateResource(client, &c.serviceEndpointSlices[i])
	}
}

func (c *cluster) deleteEndpointSlice(name string) {
	Expect(endpointSliceClientFor(c.localDynClient, c.service.Namespace).Delete(context.TODO(), name,
		metav1.DeleteOptions{})).To(Succeed())
}

func (c *cluster) createGlobalIngressIP(ingressIP *unstructured.Unstructured) {
	test.CreateResource(c.localIngressIPClient, ingressIP)
}

func (c *cluster) newHeadlessGlobalIngressIPForPod(target, ip string) *unstructured.Unstructured {
	ingressIP := c.newGlobalIngressIP("pod"+"-"+target, ip)
	Expect(unstructured.SetNestedField(ingressIP.Object, controller.HeadlessServicePod, "spec", "target")).To(Succeed())
	Expect(unstructured.SetNestedField(ingressIP.Object, target, "spec", "podRef", "name")).To(Succeed())

	return ingressIP
}

func (c *cluster) newHeadlessGlobalIngressIPForEndpointIP(name, ip, endpointIP string) *unstructured.Unstructured {
	ingressIP := c.newGlobalIngressIP("ep"+"-"+name+"-"+endpointIP, ip)
	Expect(unstructured.SetNestedField(ingressIP.Object, controller.HeadlessServiceEndpoints, "spec", "target")).To(Succeed())
	Expect(unstructured.SetNestedField(ingressIP.Object, name, "spec", "serviceRef", "name")).To(Succeed())

	annotations := map[string]string{"submariner.io/headless-svc-endpoints-ip": endpointIP}
	ingressIP.SetAnnotations(annotations)

	return ingressIP
}

func (c *cluster) newGlobalIngressIP(name, ip string) *unstructured.Unstructured {
	ingressIP := controller.GetGlobalIngressIPObj()
	ingressIP.SetName(name)
	ingressIP.SetNamespace(c.service.Namespace)
	Expect(unstructured.SetNestedField(ingressIP.Object, controller.ClusterIPService, "spec", "target")).To(Succeed())
	Expect(unstructured.SetNestedField(ingressIP.Object, c.service.Name, "spec", "serviceRef", "name")).To(Succeed())

	setIngressAllocatedIP(ingressIP, ip)
	setIngressIPConditions(ingressIP, metav1.Condition{
		Type:    "Allocated",
		Status:  metav1.ConditionTrue,
		Reason:  "Success",
		Message: "Allocated global IP",
	})

	return ingressIP
}

func (c *cluster) retrieveServiceExportCondition(se *mcsv1a1.ServiceExport, condType string) *metav1.Condition {
	obj, err := serviceExportClientFor(c.localDynClient, se.Namespace).Get(context.TODO(), se.Name, metav1.GetOptions{})
	Expect(err).To(Succeed())

	return meta.FindStatusCondition(toServiceExport(obj).Status.Conditions, condType)
}

func (c *cluster) awaitServiceExportCondition(expected ...*metav1.Condition) {
	conditionsEqual := func(actual, expected *metav1.Condition) bool {
		return actual != nil && actual.Type == expected.Type && actual.Status == expected.Status &&
			actual.Reason == expected.Reason
	}

	actual := make([]*metav1.Condition, len(expected))
	lastIndex := -1

	for i := range len(expected) - 1 {
		j := lastIndex + 1

		Eventually(func() interface{} {
			actions := c.localDynClientFake.Actions()
			for j < len(actions) {
				a := actions[j]
				j++

				if !a.Matches("update", "serviceexports") {
					continue
				}

				actual[i] = meta.FindStatusCondition(toServiceExport(a.(k8stesting.UpdateActionImpl).Object).Status.Conditions,
					expected[i].Type)

				if conditionsEqual(actual[i], expected[i]) {
					lastIndex = j

					return actual[i]
				}
			}

			return nil
		}).ShouldNot(BeNil(), "ServiceExport condition not received. Expected: "+resource.ToJSON(expected[i]))
	}

	last := len(expected) - 1

	Eventually(func() interface{} {
		obj, err := c.localServiceExportClient.Get(context.Background(), c.serviceExport.Name, metav1.GetOptions{})
		Expect(err).To(Succeed())
		se := toServiceExport(obj)

		c := meta.FindStatusCondition(se.Status.Conditions, expected[last].Type)

		if c != nil {
			Expect(c.Reason).NotTo(BeEmpty(), resource.ToJSON(c))
		}

		if conditionsEqual(c, expected[last]) {
			actual[last] = c
			return c
		}

		return nil
	}).ShouldNot(BeNil(), "ServiceExport condition not found. Expected: "+resource.ToJSON(expected[last]))

	for i := range expected {
		assertEquivalentConditions(actual[i], expected[i])
	}
}

func (c *cluster) ensureLastServiceExportCondition(expected *metav1.Condition) {
	indexOfLastCondition := func() int {
		actions := c.localDynClientFake.Actions()
		for i := len(actions) - 1; i >= 0; i-- {
			if !actions[i].Matches("update", "serviceexports") {
				continue
			}

			actual := meta.FindStatusCondition(
				toServiceExport(actions[i].(k8stesting.UpdateActionImpl).Object).Status.Conditions, expected.Type)

			if actual != nil {
				assertEquivalentConditions(actual, expected)
				return i
			}
		}

		Fail("ServiceExport condition not found. Expected: " + resource.ToJSON(expected))

		return -1
	}

	initialIndex := indexOfLastCondition()
	Consistently(func() int {
		return indexOfLastCondition()
	}).Should(Equal(initialIndex), "Expected ServiceExport condition to not change: "+resource.ToJSON(expected))
}

func (c *cluster) ensureNoServiceExportCondition(condType string, serviceExports ...*mcsv1a1.ServiceExport) {
	if len(serviceExports) == 0 {
		serviceExports = []*mcsv1a1.ServiceExport{c.serviceExport}
	}

	for _, se := range serviceExports {
		Consistently(func() interface{} {
			return c.retrieveServiceExportCondition(se, condType)
		}).Should(BeNil(), "Unexpected ServiceExport status condition")
	}
}

func (c *cluster) awaitServiceUnavailableStatus() {
	c.awaitServiceExportCondition(newServiceExportValidCondition(metav1.ConditionFalse, "ServiceUnavailable"))
}

func (c *cluster) findLocalServiceImport() *mcsv1a1.ServiceImport {
	list, err := c.localServiceImportClient.Namespace(test.LocalNamespace).List(context.TODO(), metav1.ListOptions{})
	Expect(err).To(Succeed())

	for i := range list.Items {
		if list.Items[i].GetLabels()[mcsv1a1.LabelServiceName] == c.service.Name &&
			list.Items[i].GetLabels()[constants.LabelSourceNamespace] == c.service.Namespace {
			serviceImport := toServiceImport(&list.Items[i])

			return serviceImport
		}
	}

	return nil
}

func (c *cluster) findLocalEndpointSlices() []*discovery.EndpointSlice {
	return findEndpointSlices(c.localEndpointSliceClient, c.service.Namespace, c.service.Name, c.clusterID)
}

func (c *cluster) ensureNoEndpointSlice() {
	Consistently(func() int {
		return len(findEndpointSlices(c.localEndpointSliceClient, c.service.Namespace, c.service.Name, c.clusterID))
	}, 300*time.Millisecond).Should(BeZero(), "Unexpected EndpointSlice")
}

func (c *cluster) ensureNoServiceExportActions() {
	c.localDynClientFake.ClearActions()

	Consistently(func() []string {
		return testutil.GetOccurredActionVerbs(c.localDynClientFake, "serviceexports", "get", "update")
	}, 500*time.Millisecond).Should(BeEmpty())
}

func awaitServiceImport(client dynamic.NamespaceableResourceInterface, expected *mcsv1a1.ServiceImport, ipPool *ipam.IPPool,
) *mcsv1a1.ServiceImport {
	sortSlices := func(si *mcsv1a1.ServiceImport) {
		sort.SliceStable(si.Spec.Ports, func(i, j int) bool {
			return si.Spec.Ports[i].Port < si.Spec.Ports[j].Port
		})

		sort.SliceStable(si.Status.Clusters, func(i, j int) bool {
			return si.Status.Clusters[i].Cluster < si.Status.Clusters[j].Cluster
		})
	}

	sortSlices(expected)

	expected = expected.DeepCopy()
	expectedServiceImportIPs := expected.Spec.IPs
	expected.Spec.IPs = nil

	var serviceImport *mcsv1a1.ServiceImport

	err := wait.PollUntilContextTimeout(context.Background(), 50*time.Millisecond, 5*time.Second, true,
		func(ctx context.Context) (bool, error) {
			obj, err := client.Namespace(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}

			serviceImport = toServiceImport(obj)

			sortSlices(serviceImport)

			ipsEquivalent := len(expectedServiceImportIPs) == len(serviceImport.Spec.IPs)
			actualSpec := serviceImport.Spec.DeepCopy()
			actualSpec.IPs = nil

			return ipsEquivalent && reflect.DeepEqual(&expected.Spec, actualSpec) &&
				reflect.DeepEqual(&expected.Status, &serviceImport.Status), nil
		})

	if !wait.Interrupted(err) {
		Expect(err).To(Succeed())
	}

	if serviceImport == nil {
		Fail(fmt.Sprintf("ServiceImport %s/%s not found", expected.Namespace, expected.Name))
	}

	Expect(serviceImport.Spec.IPs).To(HaveLen(len(expectedServiceImportIPs)))

	if len(serviceImport.Spec.IPs) > 0 {
		Expect(ipPool.Reserve(serviceImport.Spec.IPs...)).ToNot(Succeed(), ""+
			"ServiceImport IP was not allocated or reserved")
	}

	serviceImport.Spec.IPs = nil

	Expect(serviceImport.Spec).To(Equal(expected.Spec))
	Expect(serviceImport.Status).To(Equal(expected.Status))

	Expect(serviceImport.Labels).To(BeEmpty())

	return serviceImport
}

func getServiceImport(client dynamic.NamespaceableResourceInterface, namespace, name string) *mcsv1a1.ServiceImport {
	obj, err := client.Namespace(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	Expect(err).To(Succeed())

	return toServiceImport(obj)
}

func findEndpointSlices(client dynamic.ResourceInterface, namespace, name, clusterID string) []*discovery.EndpointSlice {
	list, err := client.List(context.TODO(), metav1.ListOptions{})
	if resource.IsMissingNamespaceErr(err) {
		return []*discovery.EndpointSlice{}
	}

	Expect(err).To(Succeed())

	var endpointSlices []*discovery.EndpointSlice

	for i := range list.Items {
		if list.Items[i].GetLabels()[mcsv1a1.LabelServiceName] == name &&
			list.Items[i].GetLabels()[constants.LabelSourceNamespace] == namespace &&
			list.Items[i].GetLabels()[mcsv1a1.LabelSourceCluster] == clusterID {
			eps := &discovery.EndpointSlice{}
			Expect(scheme.Scheme.Convert(&list.Items[i], eps, nil)).To(Succeed())

			endpointSlices = append(endpointSlices, eps)
		}
	}

	return endpointSlices
}

func awaitEndpointSlice(client dynamic.ResourceInterface, serviceName string, expected *discovery.EndpointSlice) {
	sortSlices := func(eps *discovery.EndpointSlice) {
		sort.SliceStable(eps.Ports, func(i, j int) bool {
			return *eps.Ports[i].Port < *eps.Ports[j].Port
		})

		sort.SliceStable(eps.Endpoints, func(i, j int) bool {
			return eps.Endpoints[i].Addresses[0] < eps.Endpoints[j].Addresses[0]
		})
	}

	sortSlices(expected)

	Eventually(func(g Gomega) {
		var endpointSlice *discovery.EndpointSlice

		slices := findEndpointSlices(client, expected.Namespace, serviceName, expected.Labels[mcsv1a1.LabelSourceCluster])

		for _, eps := range slices {
			if expected.Labels[constants.LabelIsHeadless] == strconv.FormatBool(true) {
				if eps.Labels[constants.LabelSourceName] == expected.Name {
					endpointSlice = eps
					break
				}
			} else if eps.AddressType == expected.AddressType {
				endpointSlice = eps
				break
			}
		}

		g.Expect(endpointSlice).NotTo(BeNil(), "EndpointSlice not found: %s", resource.ToJSON(expected))

		sortSlices(endpointSlice)

		maps.DeleteFunc(endpointSlice.Labels, func(k, _ string) bool {
			return strings.HasPrefix(k, "submariner-io/")
		})

		g.Expect(endpointSlice.Labels).To(Equal(expected.Labels), "%s EndpointSlice", expected.AddressType)

		for k, v := range expected.Annotations {
			g.Expect(endpointSlice.Annotations).To(HaveKeyWithValue(k, v), "%s EndpointSlice", expected.AddressType)
		}

		g.Expect(endpointSlice.Endpoints).To(Equal(expected.Endpoints), "%s EndpointSlice", expected.AddressType)
		g.Expect(endpointSlice.Ports).To(Equal(expected.Ports), "%s EndpointSlice", expected.AddressType)
	}).ProbeEvery(time.Millisecond * 50).Within(time.Second * 5).Should(Succeed())
}

func awaitNoEndpointSlice(client dynamic.ResourceInterface, ns, name, clusterID string) {
	Eventually(func() int {
		return len(findEndpointSlices(client, ns, name, clusterID))
	}).Should(BeZero(), "Unexpected EndpointSlice found for %s/%s", ns, name)
}

func (c *cluster) dynamicServiceClientFor() dynamic.NamespaceableResourceInterface {
	return c.localDynClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "services"})
}

func (t *testDriver) awaitAggregatedServiceImport(sType mcsv1a1.ServiceImportType, name, ns string, clusters ...*cluster) {
	expServiceImport := &mcsv1a1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", name, ns),
			Namespace: test.RemoteNamespace,
		},
		Spec: mcsv1a1.ServiceImportSpec{
			Type:                  sType,
			Ports:                 []mcsv1a1.ServicePort{},
			SessionAffinity:       t.aggregatedSessionAffinity,
			SessionAffinityConfig: t.aggregatedSessionAffinityConfig,
		},
	}

	if len(clusters) > 0 {
		if sType == mcsv1a1.ClusterSetIP {
			expServiceImport.Spec.Ports = t.aggregatedServicePorts

			if t.useClusterSetIP {
				expServiceImport.Spec.IPs = []string{"1.1.1.1"}
			}
		}

		for _, c := range clusters {
			expServiceImport.Status.Clusters = append(expServiceImport.Status.Clusters,
				mcsv1a1.ClusterStatus{Cluster: c.clusterID})
		}
	}

	actual := awaitServiceImport(t.brokerServiceImportClient, expServiceImport, t.ipPool)

	if sType == mcsv1a1.ClusterSetIP {
		Expect(actual.Annotations).To(HaveKeyWithValue(constants.UseClustersetIP, strconv.FormatBool(t.useClusterSetIP)))
	}

	expServiceImport.Name = name
	expServiceImport.Namespace = ns

	awaitServiceImport(t.cluster1.localServiceImportClient, expServiceImport, t.ipPool)
	awaitServiceImport(t.cluster2.localServiceImportClient, expServiceImport, t.ipPool)
}

func (t *testDriver) ensureAggregatedServiceImport(sType mcsv1a1.ServiceImportType, name, ns string, clusters ...*cluster) {
	Consistently(func() bool {
		t.awaitAggregatedServiceImport(sType, name, ns, clusters...)
		return true
	}).Should(BeTrue())
}

func (t *testDriver) awaitNoAggregatedServiceImport(c *cluster) {
	test.AwaitNoResource(t.brokerServiceImportClient.Namespace(test.RemoteNamespace),
		fmt.Sprintf("%s-%s", c.service.Name, c.service.Namespace))
	test.AwaitNoResource(t.cluster1.localServiceImportClient.Namespace(c.service.Namespace), c.service.Name)
	test.AwaitNoResource(t.cluster2.localServiceImportClient.Namespace(c.service.Namespace), c.service.Name)
}

func (t *testDriver) awaitEndpointSlice(c *cluster) {
	isHeadless := c.service.Spec.ClusterIP == corev1.ClusterIPNone

	epsTemplate := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.service.Namespace,
			Labels: map[string]string{
				discovery.LabelManagedBy:       constants.LabelValueManagedBy,
				mcsv1a1.LabelSourceCluster:     c.clusterID,
				mcsv1a1.LabelServiceName:       c.service.Name,
				constants.LabelSourceNamespace: c.service.Namespace,
				constants.LabelIsHeadless:      strconv.FormatBool(isHeadless),
			},
			Annotations: map[string]string{},
		},
		AddressType: discovery.AddressTypeIPv4,
	}

	for k, v := range c.service.Labels {
		epsTemplate.Labels[k] = v
	}

	var expected []discovery.EndpointSlice

	if isHeadless {
		epsTemplate.Annotations[constants.PublishNotReadyAddresses] = strconv.FormatBool(c.service.Spec.PublishNotReadyAddresses)
		epsTemplate.Annotations[constants.GlobalnetEnabled] = strconv.FormatBool(c.agentSpec.GlobalnetEnabled)

		for i := range c.headlessEndpointAddresses {
			eps := epsTemplate.DeepCopy()
			eps.Endpoints = c.headlessEndpointAddresses[i]
			eps.Ports = c.serviceEndpointSlices[i].Ports
			eps.Name = c.serviceEndpointSlices[i].Name
			eps.Labels[constants.LabelSourceName] = c.serviceEndpointSlices[i].Name
			expected = append(expected, *eps)
		}
	} else {
		for i := range c.expectedClusterIPEndpoints {
			eps := epsTemplate.DeepCopy()

			eps.Endpoints = []discovery.Endpoint{c.expectedClusterIPEndpoints[i]}

			if k8snet.IPFamilyOfString(c.expectedClusterIPEndpoints[i].Addresses[0]) == k8snet.IPv6 {
				eps.AddressType = discovery.AddressTypeIPv6
			}

			for i := range c.service.Spec.Ports {
				eps.Ports = append(eps.Ports, discovery.EndpointPort{
					Name:        &c.service.Spec.Ports[i].Name,
					Protocol:    &c.service.Spec.Ports[i].Protocol,
					Port:        &c.service.Spec.Ports[i].Port,
					AppProtocol: c.service.Spec.Ports[i].AppProtocol,
				})
			}

			expected = append(expected, *eps)
		}
	}

	for i := range expected {
		awaitEndpointSlice(t.brokerEndpointSliceClient, c.service.Name, &expected[i])
		awaitEndpointSlice(t.cluster1.localEndpointSliceClient, c.service.Name, &expected[i])
		awaitEndpointSlice(t.cluster2.localEndpointSliceClient, c.service.Name, &expected[i])
	}

	Eventually(func() []*discovery.EndpointSlice {
		return findEndpointSlices(c.localEndpointSliceClient, c.service.Namespace, c.service.Name, c.clusterID)
	}).Should(HaveLen(len(expected)))
}

func (t *testDriver) ensureEndpointSlice(c *cluster) {
	Consistently(func() bool {
		t.awaitEndpointSlice(c)
		return true
	}).Should(BeTrue())
}

func (t *testDriver) awaitNoEndpointSlice(c *cluster) {
	awaitNoEndpointSlice(t.cluster1.localEndpointSliceClient, c.service.Namespace, c.service.Name, c.clusterID)
	awaitNoEndpointSlice(t.brokerEndpointSliceClient, c.service.Namespace, c.service.Name, c.clusterID)
	awaitNoEndpointSlice(t.cluster2.localEndpointSliceClient, c.service.Namespace, c.service.Name, c.clusterID)
}

func serviceExportClientFor(client dynamic.Interface, namespace string) dynamic.ResourceInterface {
	return client.Resource(schema.GroupVersionResource{
		Group:    mcsv1a1.GroupVersion.Group,
		Version:  mcsv1a1.GroupVersion.Version,
		Resource: "serviceexports",
	}).Namespace(namespace)
}

func endpointSliceClientFor(client dynamic.Interface, namespace string) dynamic.ResourceInterface {
	return client.Resource(discovery.SchemeGroupVersion.WithResource("endpointslices")).Namespace(namespace)
}

func assertEquivalentConditions(actual, expected *metav1.Condition) {
	out := resource.ToJSON(actual)

	Expect(actual.Status).To(Equal(expected.Status), "Actual: %s", out)
	Expect(actual.LastTransitionTime).To(Not(BeNil()), "Actual: %s", out)
	Expect(actual.Reason).To(Equal(expected.Reason), "Actual: %s", out)

	if expected.Message != "" {
		Expect(actual.Message).To(ContainSubstring(expected.Message), "Actual: %s", out)
	}
}

func toServiceExport(obj interface{}) *mcsv1a1.ServiceExport {
	se := &mcsv1a1.ServiceExport{}
	Expect(scheme.Scheme.Convert(obj, se, nil)).To(Succeed())

	return se
}

func toServiceImport(obj interface{}) *mcsv1a1.ServiceImport {
	si := &mcsv1a1.ServiceImport{}
	Expect(scheme.Scheme.Convert(obj, si, nil)).To(Succeed())

	return si
}

func (t *testDriver) awaitNonHeadlessServiceExported(clusters ...*cluster) {
	t.awaitServiceExported(mcsv1a1.ClusterSetIP, clusters...)
}

func (t *testDriver) awaitHeadlessServiceExported(clusters ...*cluster) {
	t.awaitServiceExported(mcsv1a1.Headless, clusters...)
}

func (t *testDriver) awaitServiceExported(sType mcsv1a1.ServiceImportType, clusters ...*cluster) {
	t.awaitAggregatedServiceImport(sType, t.cluster1.service.Name, t.cluster1.service.Namespace, clusters...)

	for _, c := range clusters {
		t.awaitEndpointSlice(c)

		c.awaitServiceExportCondition(newServiceExportValidCondition(metav1.ConditionTrue, controller.ExportValidReason))
		c.awaitServiceExportCondition(newServiceExportReadyCondition(metav1.ConditionTrue, controller.ServiceExportedReason))
	}
}

func (t *testDriver) awaitServiceUnexported(c *cluster) {
	t.awaitNoEndpointSlice(c)

	t.awaitNoAggregatedServiceImport(c)

	c.localDynClientFake.ClearActions()

	// Ensure the service's EndpointSlices are no longer being watched by creating a EndpointSlice and verifying the
	// exported EndpointSlice isn't recreated.
	epsClient := endpointSliceClientFor(c.localDynClient, c.service.Namespace)

	_, err := epsClient.Create(context.Background(),
		resource.MustToUnstructured(&discovery.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "dummy",
				Labels: map[string]string{discovery.LabelServiceName: serviceName},
			},
		}), metav1.CreateOptions{})
	Expect(err).To(Succeed())

	c.ensureNoEndpointSlice()
	Expect(epsClient.Delete(context.Background(), "dummy", metav1.DeleteOptions{})).To(Succeed())
}

func newServiceExportValidCondition(status metav1.ConditionStatus, reason string) *metav1.Condition {
	return &metav1.Condition{
		Type:   mcsv1a1.ServiceExportValid,
		Status: status,
		Reason: reason,
	}
}

func newServiceExportReadyCondition(status metav1.ConditionStatus, reason string) *metav1.Condition {
	return &metav1.Condition{
		Type:   constants.ServiceExportReady,
		Status: status,
		Reason: reason,
	}
}

func newServiceExportConflictCondition(reason string) *metav1.Condition {
	return &metav1.Condition{
		Type:   mcsv1a1.ServiceExportConflict,
		Status: metav1.ConditionTrue,
		Reason: reason,
	}
}

func setIngressIPConditions(ingressIP *unstructured.Unstructured, conditions ...metav1.Condition) {
	var err error

	condObjs := make([]interface{}, len(conditions))
	for i := range conditions {
		condObjs[i], err = runtime.DefaultUnstructuredConverter.ToUnstructured(&conditions[i])
		Expect(err).To(Succeed())
	}

	Expect(unstructured.SetNestedSlice(ingressIP.Object, condObjs, "status", "conditions")).To(Succeed())
}

func setIngressAllocatedIP(ingressIP *unstructured.Unstructured, ip string) {
	Expect(unstructured.SetNestedField(ingressIP.Object, ip, "status", "allocatedIP")).To(Succeed())
}

func toServicePort(port mcsv1a1.ServicePort) corev1.ServicePort {
	return corev1.ServicePort{
		Name:        port.Name,
		Protocol:    port.Protocol,
		Port:        port.Port,
		AppProtocol: port.AppProtocol,
	}
}
