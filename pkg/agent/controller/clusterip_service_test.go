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
	"fmt"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/submariner-io/admiral/pkg/fake"
	"github.com/submariner-io/admiral/pkg/resource"
	"github.com/submariner-io/admiral/pkg/syncer/test"
	testutil "github.com/submariner-io/admiral/pkg/test"
	"github.com/submariner-io/lighthouse/pkg/agent/controller"
	"github.com/submariner-io/lighthouse/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

var _ = Describe("ClusterIP Service export", func() {
	Describe("in single cluster", testClusterIPServiceInOneCluster)
	Describe("in two clusters", testClusterIPServiceInTwoClusters)
	Describe("with multiple service EndpointSlices", testClusterIPServiceWithMultipleEPS)
})

//nolint:maintidx // This function composes test cases so ignore low maintainability index.
func testClusterIPServiceInOneCluster() {
	var t *testDriver

	BeforeEach(func() {
		t = newTestDiver()
	})

	JustBeforeEach(func() {
		t.justBeforeEach()

		t.cluster1.createServiceEndpointSlices()
	})

	AfterEach(func() {
		t.afterEach()
	})

	When("a ServiceExport is created", func() {
		Context("and the Service already exists", func() {
			It("should export the service and update the ServiceExport status", func() {
				t.cluster1.createService()
				t.cluster1.createServiceExport()
				t.awaitNonHeadlessServiceExported(&t.cluster1)
				t.cluster1.awaitServiceExportCondition(newServiceExportReadyCondition(metav1.ConditionFalse, controller.AwaitingExportReason),
					newServiceExportReadyCondition(metav1.ConditionTrue, controller.ServiceExportedReason))
				t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)

				By(fmt.Sprintf("Ensure cluster %q does not try to update the status for a non-existent ServiceExport",
					t.cluster2.clusterID))

				t.cluster2.ensureNoServiceExportActions()
			})
		})

		Context("and the Service doesn't initially exist", func() {
			It("should eventually export the service", func() {
				t.cluster1.createServiceExport()
				t.cluster1.awaitServiceUnavailableStatus()

				t.cluster1.createService()
				t.awaitNonHeadlessServiceExported(&t.cluster1)
			})
		})
	})

	When("a ServiceExport is deleted after the service is exported", func() {
		It("should unexport the service", func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)

			t.cluster1.deleteServiceExport()
			t.awaitServiceUnexported(&t.cluster1)
		})
	})

	When("an exported Service is deleted and recreated while the ServiceExport still exists", func() {
		It("should unexport and re-export the service", func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)
			t.cluster1.localDynClientFake.ClearActions()

			By("Deleting the service")
			t.cluster1.deleteService()
			t.cluster1.awaitServiceUnavailableStatus()
			t.cluster1.awaitServiceExportCondition(newServiceExportReadyCondition(metav1.ConditionFalse, controller.NoServiceImportReason))
			t.awaitServiceUnexported(&t.cluster1)

			By("Re-creating the service")
			t.cluster1.createService()
			t.awaitNonHeadlessServiceExported(&t.cluster1)
		})
	})

	When("the type of an exported Service is updated to an unsupported type", func() {
		It("should unexport the ServiceImport and update the ServiceExport status appropriately", func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)

			t.cluster1.service.Spec.Type = corev1.ServiceTypeNodePort
			t.cluster1.updateService()

			t.cluster1.awaitServiceExportCondition(newServiceExportValidCondition(metav1.ConditionFalse, "UnsupportedServiceType"))
			t.cluster1.awaitServiceExportCondition(newServiceExportReadyCondition(metav1.ConditionFalse, controller.NoServiceImportReason))
			t.awaitServiceUnexported(&t.cluster1)
		})
	})

	When("a ServiceExport is created for a Service whose type is unsupported", func() {
		BeforeEach(func() {
			t.cluster1.service.Spec.Type = corev1.ServiceTypeNodePort
		})

		JustBeforeEach(func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
		})

		It("should update the ServiceExport status appropriately and not export the serviceImport", func() {
			t.cluster1.awaitServiceExportCondition(newServiceExportValidCondition(metav1.ConditionFalse, "UnsupportedServiceType"))
			t.cluster1.ensureNoServiceExportCondition(constants.ServiceExportReady)
		})

		Context("and is subsequently updated to a supported type", func() {
			It("should eventually export the service and update the ServiceExport status appropriately", func() {
				t.cluster1.awaitServiceExportCondition(newServiceExportValidCondition(metav1.ConditionFalse, "UnsupportedServiceType"))

				t.cluster1.service.Spec.Type = corev1.ServiceTypeClusterIP
				t.cluster1.updateService()

				t.awaitNonHeadlessServiceExported(&t.cluster1)
			})
		})
	})

	When("the backend service EndpointSlice has no ready addresses", func() {
		JustBeforeEach(func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)
		})

		Specify("the exported EndpointSlice's service IP address should indicate not ready", func() {
			for i := range t.cluster1.serviceEndpointSlices[0].Endpoints {
				t.cluster1.serviceEndpointSlices[0].Endpoints[i].Conditions = discovery.EndpointConditions{Ready: ptr.To(false)}
			}

			t.cluster1.expectedClusterIPEndpoints[0].Conditions = discovery.EndpointConditions{Ready: ptr.To(false)}

			t.cluster1.updateServiceEndpointSlices()
			t.ensureEndpointSlice(&t.cluster1)
		})
	})

	When("the ports for an exported service are updated", func() {
		JustBeforeEach(func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)
		})

		It("should re-export the service with the updated ports", func() {
			t.cluster1.service.Spec.Ports = append(t.cluster1.service.Spec.Ports, toServicePort(port3))
			t.aggregatedServicePorts = append(t.aggregatedServicePorts, port3)

			t.cluster1.updateService()
			t.awaitNonHeadlessServiceExported(&t.cluster1)
		})
	})

	When("the labels for an exported service are updated", func() {
		JustBeforeEach(func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)
		})

		It("should update the existing EndpointSlice labels", func() {
			existingEPS := findEndpointSlices(t.cluster1.localEndpointSliceClient, t.cluster1.service.Namespace,
				t.cluster1.service.Name, t.cluster1.clusterID)[0]

			By("Updating service labels")

			newLabelName := "new-label"
			newLabelValue := "new-value"

			t.cluster1.service.Labels[newLabelName] = newLabelValue
			t.cluster1.serviceEndpointSlices[0].Labels[newLabelName] = newLabelValue

			t.cluster1.updateServiceEndpointSlices()

			Eventually(func() map[string]string {
				eps, err := t.cluster1.localEndpointSliceClient.Get(context.TODO(), existingEPS.Name, metav1.GetOptions{})
				Expect(err).To(Succeed())

				return eps.GetLabels()
			}).Should(HaveKeyWithValue(newLabelName, newLabelValue))

			newSlices := findEndpointSlices(t.cluster1.localEndpointSliceClient, t.cluster1.service.Namespace,
				t.cluster1.service.Name, t.cluster1.clusterID)
			Expect(newSlices).To(HaveLen(1))
			Expect(newSlices[0].Name).To(Equal(existingEPS.Name))
		})
	})

	When("the session affinity is configured for an exported service", func() {
		BeforeEach(func() {
			t.cluster1.service.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			t.cluster1.service.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: ptr.To(int32(10))},
			}

			t.aggregatedSessionAffinity = t.cluster1.service.Spec.SessionAffinity
			t.aggregatedSessionAffinityConfig = t.cluster1.service.Spec.SessionAffinityConfig
		})

		It("should be propagated to the ServiceImport", func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)
		})
	})

	Context("with clusterset IP enabled", func() {
		BeforeEach(func() {
			t.useClusterSetIP = true
		})

		JustBeforeEach(func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
		})

		Context("via ServiceExport annotation", func() {
			BeforeEach(func() {
				t.cluster1.serviceExport.Annotations = map[string]string{constants.UseClustersetIP: strconv.FormatBool(true)}
			})

			It("should allocate an IP for the aggregated ServiceImport and release the IP when unexported", func() {
				t.awaitNonHeadlessServiceExported(&t.cluster1)

				localSI := getServiceImport(t.cluster1.localServiceImportClient, t.cluster1.service.Namespace, t.cluster1.service.Name)
				Expect(localSI.Annotations).To(HaveKeyWithValue(constants.ClustersetIPAllocatedBy, t.cluster1.clusterID))

				By("Unexporting the service")

				t.cluster1.deleteServiceExport()

				Eventually(func() error {
					return t.ipPool.Reserve(localSI.Spec.IPs...)
				}).Should(Succeed(), "ServiceImport IP was not released")
			})

			Context("but with no IP pool specified", func() {
				BeforeEach(func() {
					t.useClusterSetIP = false
					t.ipPool = nil
				})

				It("should not set the IP on the aggregated ServiceImport", func() {
					t.awaitNonHeadlessServiceExported(&t.cluster1)
				})
			})

			Context("with the IP pool initially exhausted", func() {
				var ips []string

				BeforeEach(func() {
					var err error

					ips, err = t.ipPool.Allocate(t.ipPool.Size())
					Expect(err).To(Succeed())
				})

				It("should eventually set the IP on the aggregated ServiceImport", func() {
					t.cluster1.awaitServiceExportCondition(newServiceExportReadyCondition(metav1.ConditionFalse,
						controller.ExportFailedReason))

					_ = t.ipPool.Release(ips...)

					t.awaitNonHeadlessServiceExported(&t.cluster1)
				})
			})
		})

		Context("via the global setting", func() {
			BeforeEach(func() {
				t.cluster1.agentSpec.ClustersetIPEnabled = true
			})

			It("should set the IP on the aggregated ServiceImport", func() {
				t.awaitNonHeadlessServiceExported(&t.cluster1)
			})

			Context("but disabled via ServiceExport annotation", func() {
				BeforeEach(func() {
					t.useClusterSetIP = false
					t.cluster1.serviceExport.Annotations = map[string]string{constants.UseClustersetIP: strconv.FormatBool(false)}
				})

				It("should not set the IP on the aggregated ServiceImport", func() {
					t.awaitNonHeadlessServiceExported(&t.cluster1)
				})
			})
		})
	})

	When("two Services with the same name in different namespaces are exported", func() {
		It("should correctly export both services", func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)

			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      t.cluster1.service.Name,
					Namespace: "other-service-ns",
				},
				Spec: corev1.ServiceSpec{
					ClusterIPs: []string{"10.253.9.2"},
				},
			}

			serviceExport := &mcsv1a1.ServiceExport{
				ObjectMeta: metav1.ObjectMeta{
					Name:      service.Name,
					Namespace: service.Namespace,
				},
			}

			serviceEPS := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:   service.Name + "-abcde",
					Labels: map[string]string{discovery.LabelServiceName: serviceName},
				},
				AddressType: discovery.AddressTypeIPv4,
			}

			expServiceImport := &mcsv1a1.ServiceImport{
				ObjectMeta: metav1.ObjectMeta{
					Name:      service.Name,
					Namespace: service.Namespace,
				},
				Spec: mcsv1a1.ServiceImportSpec{
					Type:  mcsv1a1.ClusterSetIP,
					Ports: []mcsv1a1.ServicePort{},
				},
				Status: mcsv1a1.ServiceImportStatus{
					Clusters: []mcsv1a1.ClusterStatus{
						{
							Cluster: t.cluster1.clusterID,
						},
					},
				},
			}

			expEndpointSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      service.Name,
					Namespace: service.Namespace,
					Labels: map[string]string{
						discovery.LabelManagedBy:       constants.LabelValueManagedBy,
						mcsv1a1.LabelSourceCluster:     t.cluster1.clusterID,
						mcsv1a1.LabelServiceName:       service.Name,
						constants.LabelSourceNamespace: service.Namespace,
						constants.LabelIsHeadless:      strconv.FormatBool(false),
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Addresses:  []string{service.Spec.ClusterIPs[0]},
						Conditions: discovery.EndpointConditions{Ready: ptr.To(false)},
					},
				},
			}

			test.CreateResource(endpointSliceClientFor(t.cluster1.localDynClient, service.Namespace), serviceEPS)
			test.CreateResource(t.cluster1.dynamicServiceClientFor().Namespace(service.Namespace), service)
			test.CreateResource(serviceExportClientFor(t.cluster1.localDynClient, service.Namespace), serviceExport)

			awaitServiceImport(t.cluster2.localServiceImportClient, expServiceImport, t.ipPool)
			awaitEndpointSlice(endpointSliceClientFor(t.cluster2.localDynClient, service.Namespace), service.Name, expEndpointSlice)

			// Ensure the resources for the first Service weren't overwritten
			t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace, &t.cluster1)

			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict, serviceExport)
		})
	})

	Specify("an EndpointSlice not managed by Lighthouse should not be synced to the broker", func() {
		test.CreateResource(endpointSliceClientFor(t.cluster1.localDynClient, t.cluster1.service.Namespace),
			&discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Name:   "other-eps",
				Labels: map[string]string{discovery.LabelManagedBy: "other"},
			}})

		testutil.EnsureNoResource(resource.ForDynamic(endpointSliceClientFor(t.syncerConfig.BrokerClient,
			test.RemoteNamespace)), "other-eps")
	})

	When("the namespace of an exported service does not initially exist on an importing cluster", func() {
		createNamespace := func(dynClient dynamic.Interface, name string) {
			test.CreateResource(dynClient.Resource(corev1.SchemeGroupVersion.WithResource("namespaces")), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
				},
			})
		}

		BeforeEach(func() {
			fake.AddVerifyNamespaceReactor(t.cluster2.localDynClientFake, "serviceimports", "endpointslices")

			createNamespace(t.cluster2.localDynClient, test.LocalNamespace)
		})

		JustBeforeEach(func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
		})

		It("should eventually import the service when the namespace is created", func() {
			expServiceImport := &mcsv1a1.ServiceImport{
				ObjectMeta: metav1.ObjectMeta{
					Name:      t.cluster1.service.Name,
					Namespace: t.cluster1.service.Namespace,
				},
				Spec: mcsv1a1.ServiceImportSpec{
					Type:            mcsv1a1.ClusterSetIP,
					Ports:           t.aggregatedServicePorts,
					SessionAffinity: corev1.ServiceAffinityNone,
				},
				Status: mcsv1a1.ServiceImportStatus{
					Clusters: []mcsv1a1.ClusterStatus{{Cluster: t.cluster1.clusterID}},
				},
			}

			awaitServiceImport(t.cluster1.localServiceImportClient, expServiceImport, t.ipPool)

			testutil.EnsureNoResource(resource.ForDynamic(t.cluster2.localServiceImportClient.Namespace(
				t.cluster1.service.Namespace)), t.cluster1.service.Name)
			t.cluster2.ensureNoEndpointSlice()

			By("Creating namespace on importing cluster")

			createNamespace(t.cluster2.localDynClient, t.cluster1.service.Namespace)

			t.awaitNonHeadlessServiceExported(&t.cluster1)
		})
	})

	When("an existing ServiceExport has the legacy Synced status condition", func() {
		BeforeEach(func() {
			t.cluster1.serviceExport.Status.Conditions = []metav1.Condition{
				{
					Type:    "Synced",
					Status:  metav1.ConditionTrue,
					Message: "Service was successfully exported to the broker",
				},
			}
		})

		It("should be migrated to the Ready status condition", func() {
			t.cluster1.createService()
			t.cluster1.createServiceExport()
			t.awaitNonHeadlessServiceExported(&t.cluster1)
			t.cluster1.ensureNoServiceExportCondition("Synced")
		})
	})
}

//nolint:maintidx // This function composes test cases so ignore low maintainability index.
func testClusterIPServiceInTwoClusters() {
	noConflictCondition := &metav1.Condition{
		Type:   mcsv1a1.ServiceExportConflict,
		Status: metav1.ConditionFalse,
		Reason: controller.NoConflictsReason,
	}

	var t *testDriver

	BeforeEach(func() {
		t = newTestDiver()
	})

	JustBeforeEach(func() {
		t.cluster1.start(t, *t.syncerConfig)

		t.cluster1.createServiceEndpointSlices()
		t.cluster1.createService()
		t.cluster1.createServiceExport()

		// Sleep a little before starting the second cluster to ensure its resource CreationTimestamps will be
		// later than the first cluster to ensure conflict checking is deterministic.
		time.Sleep(100 * time.Millisecond)

		t.cluster2.start(t, *t.syncerConfig)

		t.cluster2.createServiceEndpointSlices()
		t.cluster2.createService()
		t.cluster2.createServiceExport()
	})

	AfterEach(func() {
		t.afterEach()
	})

	Context("", func() {
		BeforeEach(func() {
			t.cluster1.service.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			t.cluster1.service.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: ptr.To(int32(10))},
			}

			t.cluster2.service.Spec.SessionAffinity = t.cluster1.service.Spec.SessionAffinity
			t.cluster2.service.Spec.SessionAffinityConfig = t.cluster1.service.Spec.SessionAffinityConfig

			t.aggregatedSessionAffinity = t.cluster1.service.Spec.SessionAffinity
			t.aggregatedSessionAffinityConfig = t.cluster1.service.Spec.SessionAffinityConfig
		})

		It("should export the service in both clusters", func() {
			t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)
			t.cluster1.ensureLastServiceExportCondition(newServiceExportReadyCondition(metav1.ConditionTrue,
				controller.ServiceExportedReason))
			t.cluster1.ensureLastServiceExportCondition(newServiceExportValidCondition(metav1.ConditionTrue, controller.ExportValidReason))
			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
			t.cluster2.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)

			By("Ensure conflict checking does not try to unnecessarily update the ServiceExport status")

			t.cluster1.ensureNoServiceExportActions()
		})
	})

	Context("with differing ports", func() {
		BeforeEach(func() {
			t.cluster2.service.Spec.Ports = []corev1.ServicePort{toServicePort(port1), toServicePort(port3)}
			t.aggregatedServicePorts = []mcsv1a1.ServicePort{port1, port2, port3}
		})

		It("should correctly set the ports in the aggregated ServiceImport and set the Conflict status condition", func() {
			t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)

			condition := newServiceExportConflictCondition(controller.PortConflictReason)
			t.cluster1.awaitServiceExportCondition(condition)
			t.cluster2.awaitServiceExportCondition(condition)
		})

		Context("and after unexporting from one cluster", func() {
			It("should correctly update the ports in the aggregated ServiceImport and clear the Conflict status condition", func() {
				t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)

				t.aggregatedServicePorts = []mcsv1a1.ServicePort{port1, port3}
				t.cluster1.deleteServiceExport()

				t.awaitNoEndpointSlice(&t.cluster1)
				t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster2.service.Name, t.cluster2.service.Namespace, &t.cluster2)
				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})

		Context("initially and after updating the ports to match", func() {
			It("should correctly update the ports in the aggregated ServiceImport and clear the Conflict status condition", func() {
				t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)
				t.cluster1.awaitServiceExportCondition(newServiceExportConflictCondition(controller.PortConflictReason))

				t.aggregatedServicePorts = []mcsv1a1.ServicePort{port1, port2}
				t.cluster2.service.Spec.Ports = []corev1.ServicePort{toServicePort(port1), toServicePort(port2)}
				t.cluster2.updateService()

				t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)
				t.cluster1.awaitServiceExportCondition(noConflictCondition)
				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})
	})

	Context("with conflicting ports", func() {
		BeforeEach(func() {
			t.cluster2.service.Spec.Ports = []corev1.ServicePort{t.cluster1.service.Spec.Ports[0], toServicePort(port3)}
			t.cluster2.service.Spec.Ports[0].Port++
			t.aggregatedServicePorts = []mcsv1a1.ServicePort{port1, port2, port3}
		})

		It("should correctly set the ports in the aggregated ServiceImport and set the Conflict status condition", func() {
			t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)

			condition := newServiceExportConflictCondition(controller.PortConflictReason)
			t.cluster1.awaitServiceExportCondition(condition)
			t.cluster2.awaitServiceExportCondition(condition)
		})
	})

	Context("with differing service types", func() {
		BeforeEach(func() {
			t.cluster2.service.Spec.ClusterIP = corev1.ClusterIPNone
		})

		It("should set the Conflict status condition on the second cluster and not export it", func() {
			t.cluster2.ensureNoEndpointSlice()
			t.awaitNonHeadlessServiceExported(&t.cluster1)

			t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(controller.TypeConflictReason))
			t.cluster2.awaitServiceExportCondition(newServiceExportReadyCondition(metav1.ConditionFalse, controller.ExportFailedReason))
			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
		})

		Context("initially and after updating the service types to match", func() {
			It("should export the service in both clusters", func() {
				t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(controller.TypeConflictReason))

				t.cluster2.service.Spec.ClusterIP = t.cluster2.expectedClusterIPEndpoints[0].Addresses[0]
				t.cluster2.updateService()

				t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)
				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})
	})

	Context("with differing service SessionAffinity", func() {
		BeforeEach(func() {
			t.cluster1.service.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			t.aggregatedSessionAffinity = t.cluster1.service.Spec.SessionAffinity
		})

		It("should resolve the conflict and set the Conflict status condition on the conflicting cluster", func() {
			t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace,
				&t.cluster1, &t.cluster2)

			t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
				controller.SessionAffinityConflictReason))
			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
		})

		Context("initially and after updating the SessionAffinity on the conflicting cluster to match", func() {
			It("should clear the Conflict status condition on the conflicting cluster", func() {
				t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
					controller.SessionAffinityConflictReason))

				By("Updating the SessionAffinity on the service")

				t.cluster2.service.Spec.SessionAffinity = t.cluster1.service.Spec.SessionAffinity
				t.cluster2.updateService()

				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})

		Context("initially and after updating the SessionAffinity on the oldest exporting cluster to match", func() {
			It("should clear the Conflict status condition on the conflicting cluster", func() {
				t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
					controller.SessionAffinityConflictReason))

				By("Updating the SessionAffinity on the service")

				t.cluster1.service.Spec.SessionAffinity = t.cluster2.service.Spec.SessionAffinity
				t.cluster1.updateService()

				t.aggregatedSessionAffinity = t.cluster1.service.Spec.SessionAffinity
				t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace,
					&t.cluster1, &t.cluster2)

				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})

		Context("initially and after the service on the oldest exporting cluster is unexported", func() {
			It("should update the SessionAffinity on the aggregated ServiceImport and clear the Conflict status condition", func() {
				t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
					controller.SessionAffinityConflictReason))

				By("Unexporting the service")

				t.cluster1.deleteServiceExport()

				t.aggregatedSessionAffinity = t.cluster2.service.Spec.SessionAffinity
				t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace, &t.cluster2)
				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})
	})

	Context("with differing service SessionAffinityConfig", func() {
		BeforeEach(func() {
			t.cluster1.service.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			t.cluster2.service.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			t.aggregatedSessionAffinity = t.cluster1.service.Spec.SessionAffinity

			t.cluster1.service.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: ptr.To(int32(10))},
			}
			t.aggregatedSessionAffinityConfig = t.cluster1.service.Spec.SessionAffinityConfig
		})

		It("should resolve the conflict and set the Conflict status condition on the conflicting cluster", func() {
			t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace,
				&t.cluster1, &t.cluster2)

			t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
				controller.SessionAffinityConfigConflictReason))
			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
		})

		Context("initially and after updating the SessionAffinityConfig on the conflicting cluster to match", func() {
			It("should clear the Conflict status condition on the conflicting cluster", func() {
				t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
					controller.SessionAffinityConfigConflictReason))

				By("Updating the SessionAffinityConfig on the service")

				t.cluster2.service.Spec.SessionAffinityConfig = t.cluster1.service.Spec.SessionAffinityConfig
				t.cluster2.updateService()

				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})

		Context("initially and after updating the SessionAffinityConfig on the oldest exporting cluster to match", func() {
			It("should clear the Conflict status condition on the conflicting cluster", func() {
				t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
					controller.SessionAffinityConfigConflictReason))

				By("Updating the SessionAffinityConfig on the service")

				t.cluster1.service.Spec.SessionAffinityConfig = t.cluster2.service.Spec.SessionAffinityConfig
				t.cluster1.updateService()

				t.aggregatedSessionAffinityConfig = t.cluster1.service.Spec.SessionAffinityConfig
				t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace,
					&t.cluster1, &t.cluster2)

				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})

		Context("initially and after the service on the oldest exporting cluster is unexported", func() {
			BeforeEach(func() {
				t.cluster2.service.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
					ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: ptr.To(int32(20))},
				}
			})

			It("should update the SessionAffinity on the aggregated ServiceImport and clear the Conflict status condition", func() {
				t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
					controller.SessionAffinityConfigConflictReason))

				By("Unexporting the service")

				t.cluster1.deleteServiceExport()

				t.aggregatedSessionAffinityConfig = t.cluster2.service.Spec.SessionAffinityConfig
				t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace, &t.cluster2)
				t.cluster2.awaitServiceExportCondition(noConflictCondition)
			})
		})
	})

	Context("with differing service SessionAffinity and SessionAffinityConfig", func() {
		BeforeEach(func() {
			t.cluster1.service.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			t.aggregatedSessionAffinity = t.cluster1.service.Spec.SessionAffinity

			t.cluster1.service.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{TimeoutSeconds: ptr.To(int32(10))},
			}
			t.aggregatedSessionAffinityConfig = t.cluster1.service.Spec.SessionAffinityConfig
		})

		It("should resolve the conflicts and set the Conflict status condition on the conflicting cluster", func() {
			t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace,
				&t.cluster1, &t.cluster2)

			t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(
				fmt.Sprintf("%s,%s", controller.SessionAffinityConflictReason, controller.SessionAffinityConfigConflictReason)))
			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
		})
	})

	Context("with clusterset IP enabled on the first exporting cluster but not the second", func() {
		BeforeEach(func() {
			t.useClusterSetIP = true
			t.cluster1.serviceExport.Annotations = map[string]string{constants.UseClustersetIP: strconv.FormatBool(true)}
		})

		JustBeforeEach(func() {
			t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)
		})

		It("should set the Conflict status condition on the second cluster", func() {
			t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(controller.ClusterSetIPEnablementConflictReason))
			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)

			By("Updating the ServiceExport on the second cluster")

			se, err := t.cluster2.localServiceExportClient.Get(context.TODO(), t.cluster2.serviceExport.Name, metav1.GetOptions{})
			Expect(err).To(Succeed())

			se.SetAnnotations(map[string]string{constants.UseClustersetIP: strconv.FormatBool(true)})
			test.UpdateResource(t.cluster2.localServiceExportClient, se)

			t.cluster2.awaitServiceExportCondition(noConflictCondition)
		})

		It("should not release the allocated clusterset IP until all clusters have unexported", func() {
			localSI := getServiceImport(t.cluster1.localServiceImportClient, t.cluster1.service.Namespace, t.cluster1.service.Name)

			By("Unexporting service on the first cluster")

			t.cluster1.deleteServiceExport()

			t.awaitNoEndpointSlice(&t.cluster1)
			t.awaitAggregatedServiceImport(mcsv1a1.ClusterSetIP, t.cluster1.service.Name, t.cluster1.service.Namespace, &t.cluster2)

			Consistently(func() error {
				return t.ipPool.Reserve(localSI.Spec.IPs...)
			}).ShouldNot(Succeed(), "ServiceImport IP was released")

			By("Unexporting service on the second cluster")

			t.cluster2.deleteServiceExport()

			t.awaitServiceUnexported(&t.cluster2)

			Eventually(func() error {
				return t.ipPool.Reserve(localSI.Spec.IPs...)
			}).Should(Succeed(), "ServiceImport IP was not released")
		})
	})

	Context("with clusterset IP disabled on the first exporting cluster but enabled on the second", func() {
		BeforeEach(func() {
			t.cluster2.serviceExport.Annotations = map[string]string{constants.UseClustersetIP: strconv.FormatBool(true)}
		})

		It("should set the Conflict status condition on the second cluster", func() {
			t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)
			t.cluster2.awaitServiceExportCondition(newServiceExportConflictCondition(controller.ClusterSetIPEnablementConflictReason))
			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
		})
	})

	Context("with clusterset IP enabled on both clusters", func() {
		BeforeEach(func() {
			t.useClusterSetIP = true
			t.cluster1.serviceExport.Annotations = map[string]string{constants.UseClustersetIP: strconv.FormatBool(true)}
			t.cluster2.serviceExport.Annotations = map[string]string{constants.UseClustersetIP: strconv.FormatBool(true)}
		})

		Specify("the first cluster should allocate the clusterset IP", func() {
			t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)

			localSI := getServiceImport(t.cluster1.localServiceImportClient, t.cluster1.service.Namespace, t.cluster1.service.Name)
			Expect(localSI.Annotations).To(HaveKeyWithValue(constants.ClustersetIPAllocatedBy, t.cluster1.clusterID))

			t.cluster1.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
			t.cluster2.ensureNoServiceExportCondition(mcsv1a1.ServiceExportConflict)
		})

		Context("with differing ports", func() {
			BeforeEach(func() {
				t.cluster2.service.Spec.Ports = []corev1.ServicePort{toServicePort(port1), toServicePort(port3)}
				t.aggregatedServicePorts = []mcsv1a1.ServicePort{port1, port2, port3}
			})

			It("should correctly set the Conflict status condition", func() {
				t.awaitNonHeadlessServiceExported(&t.cluster1, &t.cluster2)

				t.cluster1.awaitServiceExportCondition(newServiceExportConflictCondition(controller.PortConflictReason))

				Expect(t.cluster1.retrieveServiceExportCondition(t.cluster1.serviceExport, mcsv1a1.ServiceExportConflict).Message).
					To(ContainSubstring("expose the union"))
			})
		})
	})
}

func testClusterIPServiceWithMultipleEPS() {
	var t *testDriver

	BeforeEach(func() {
		t = newTestDiver()

		t.cluster1.createService()
		t.cluster1.createServiceExport()
	})

	JustBeforeEach(func() {
		t.justBeforeEach()
	})

	AfterEach(func() {
		t.afterEach()
	})

	Specify("the exported EndpointSlice should be correctly updated as backend service EndpointSlices are created/updated/deleted", func() {
		By("Creating initial service EndpointSlice with no ready endpoints")

		t.cluster1.expectedClusterIPEndpoints[0].Conditions = discovery.EndpointConditions{Ready: ptr.To(false)}

		t.cluster1.serviceEndpointSlices[0].Endpoints = []discovery.Endpoint{
			{
				Addresses:  []string{epIP1},
				Conditions: discovery.EndpointConditions{Ready: ptr.To(false)},
			},
		}

		t.cluster1.createServiceEndpointSlices()
		t.awaitNonHeadlessServiceExported(&t.cluster1)

		By("Creating service EndpointSlice with ready endpoint")

		t.cluster1.expectedClusterIPEndpoints[0].Conditions = discovery.EndpointConditions{Ready: ptr.To(true)}

		t.cluster1.serviceEndpointSlices = append(t.cluster1.serviceEndpointSlices, discovery.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:   fmt.Sprintf("%s-%s2", serviceName, clusterID1),
				Labels: t.cluster1.serviceEndpointSlices[0].Labels,
			},
			AddressType: discovery.AddressTypeIPv4,
			Endpoints: []discovery.Endpoint{
				{
					Addresses:  []string{epIP2},
					Conditions: discovery.EndpointConditions{Ready: ptr.To(true)},
				},
			},
		})

		t.cluster1.createServiceEndpointSlices()
		t.ensureEndpointSlice(&t.cluster1)

		By("Deleting service EndpointSlice with ready endpoint")

		t.cluster1.deleteEndpointSlice(t.cluster1.serviceEndpointSlices[1].Name)

		t.cluster1.expectedClusterIPEndpoints[0].Conditions = discovery.EndpointConditions{Ready: ptr.To(false)}
		t.cluster1.serviceEndpointSlices = t.cluster1.serviceEndpointSlices[:1]

		t.ensureEndpointSlice(&t.cluster1)
	})
}
