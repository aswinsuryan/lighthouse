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

package resolver_test

import (
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/submariner-io/lighthouse/coredns/constants"
	"github.com/submariner-io/lighthouse/coredns/resolver"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8snet "k8s.io/utils/net"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

var _ = Describe("GetDNSRecords", func() {
	Describe("Headless Service", func() {
		Describe("", testHeadlessService)
		When("a service is present in multiple clusters", testHeadlessServiceInMultipleClusters)
	})
})

func testHeadlessService() {
	t := newTestDriver()

	endpoint1DNSRecord := resolver.DNSRecord{
		IP:          endpointIP1,
		Ports:       []mcsv1a1.ServicePort{port1},
		ClusterName: clusterID1,
		HostName:    endpointHostname1,
	}

	endpoint2DNSRecord := resolver.DNSRecord{
		IP:          endpointIP2,
		Ports:       []mcsv1a1.ServicePort{port1},
		ClusterName: clusterID1,
		HostName:    endpointHostname2,
	}

	endpoint3DNSRecord := resolver.DNSRecord{
		IP:          endpointIP3,
		Ports:       []mcsv1a1.ServicePort{port1},
		ClusterName: clusterID1,
		HostName:    endpointHostname3,
	}

	endpoint4DNSRecord := resolver.DNSRecord{
		IP:          endpointIP4,
		Ports:       []mcsv1a1.ServicePort{port1},
		ClusterName: clusterID1,
		HostName:    endpointHostname4,
	}

	var (
		endpointSlice *discovery.EndpointSlice
		annotations   map[string]string
	)

	BeforeEach(func() {
		t.resolver.PutServiceImport(newHeadlessAggregatedServiceImport(namespace1, service1))

		annotations = nil
		endpointSlice = nil
	})

	JustBeforeEach(func() {
		endpointSlice.Annotations = annotations
		t.putEndpointSlice(endpointSlice)
	})

	When("a service has both ready and not-ready addresses", func() {
		BeforeEach(func() {
			endpointSlice = newEndpointSlice(namespace1, service1, clusterID1, []mcsv1a1.ServicePort{port1},
				discovery.Endpoint{
					Addresses:  []string{endpointIP1},
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
				discovery.Endpoint{
					Addresses:  []string{endpointIP2},
					Conditions: discovery.EndpointConditions{Ready: &notReady},
				},
				discovery.Endpoint{
					Addresses:  []string{endpointIP3},
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
				discovery.Endpoint{
					Addresses:  []string{endpointIP4},
					Conditions: discovery.EndpointConditions{Ready: &notReady},
				},
			)
		})

		Context("and the publish-not-ready-addresses annotation is not present", func() {
			It("should return DNS records for only the ready addresses", func() {
				t.assertDNSRecordsFound(namespace1, service1, "", "", k8snet.IPv4, true,
					endpoint1DNSRecord,
					endpoint3DNSRecord,
				)
			})
		})

		Context("and the publish-not-ready-addresses annotation is set to false", func() {
			BeforeEach(func() {
				annotations = map[string]string{constants.PublishNotReadyAddresses: strconv.FormatBool(false)}
			})

			It("should return DNS records for only the ready addresses", func() {
				t.assertDNSRecordsFound(namespace1, service1, "", "", k8snet.IPv4, true,
					endpoint1DNSRecord,
					endpoint3DNSRecord,
				)
			})
		})

		Context("and the publish-not-ready-addresses annotation is set to true", func() {
			BeforeEach(func() {
				annotations = map[string]string{constants.PublishNotReadyAddresses: strconv.FormatBool(true)}
			})

			It("should return all the DNS records", func() {
				t.assertDNSRecordsFound(namespace1, service1, "", "", k8snet.IPv4, true,
					endpoint1DNSRecord,
					endpoint2DNSRecord,
					endpoint3DNSRecord,
					endpoint4DNSRecord,
				)
			})
		})
	})

	When("a service is on the local cluster", func() {
		BeforeEach(func() {
			t.clusterStatus.SetLocalClusterID(clusterID1)

			endpointSlice = newEndpointSlice(namespace1, service1, clusterID1, []mcsv1a1.ServicePort{port1},
				discovery.Endpoint{
					Addresses:  []string{endpointIP1},
					NodeName:   &nodeName1,
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
			)
		})

		Context("and globalnet is enabled", func() {
			BeforeEach(func() {
				annotations = map[string]string{
					constants.GlobalnetEnabled:         strconv.FormatBool(true),
					constants.PublishNotReadyAddresses: strconv.FormatBool(true),
				}

				// If the local cluster EndpointSlice is created before the local K8s EndpointSlice, PutEndpointSlices should
				// return true to requeue.
				eps := newEndpointSlice(namespace1, service1, clusterID1, nil)
				eps.Annotations = annotations
				Expect(t.resolver.PutEndpointSlices(eps)).To(BeTrue())

				eps1 := &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      service1 + "local-1",
						Namespace: namespace1,
						Labels: map[string]string{
							discovery.LabelServiceName: service1,
						},
					},
					Ports: []discovery.EndpointPort{{
						Name:        &port1.Name,
						Protocol:    &port1.Protocol,
						Port:        &port1.Port,
						AppProtocol: port1.AppProtocol,
					}},
					Endpoints: []discovery.Endpoint{
						{
							Addresses:  []string{endpointIP2},
							NodeName:   &nodeName1,
							Conditions: discovery.EndpointConditions{Ready: &notReady},
						},
					},
				}
				t.createEndpointSlice(eps1)

				t.createEndpointSlice(&discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      service1 + "local-2",
						Namespace: eps1.Namespace,
						Labels:    eps1.Labels,
					},
					Ports: eps1.Ports,
					Endpoints: []discovery.Endpoint{
						{
							Addresses:  []string{endpointIP3},
							NodeName:   &nodeName1,
							Conditions: discovery.EndpointConditions{Ready: &ready},
						},
					},
				})
			})

			It("should return the local DNS records", func() {
				t.assertDNSRecordsFound(namespace1, service1, clusterID1, "", k8snet.IPv4, true,
					endpoint2DNSRecord,
					endpoint3DNSRecord)
			})
		})

		Context("and globalnet is disabled", func() {
			BeforeEach(func() {
				annotations = map[string]string{constants.GlobalnetEnabled: strconv.FormatBool(false)}
			})

			It("should return the global DNS records", func() {
				t.assertDNSRecordsFound(namespace1, service1, clusterID1, "", k8snet.IPv4, true, endpoint1DNSRecord)
			})
		})
	})

	When("a service has multiple EndpointSlices with duplicate addresses", func() {
		BeforeEach(func() {
			endpointSlice = newEndpointSlice(namespace1, service1, clusterID1, []mcsv1a1.ServicePort{port1},
				discovery.Endpoint{
					Addresses:  []string{endpointIP1},
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
				discovery.Endpoint{
					Addresses:  []string{endpointIP2},
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
				discovery.Endpoint{
					Addresses:  []string{endpointIP3},
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
			)
		})

		JustBeforeEach(func() {
			Expect(t.resolver.PutEndpointSlices(endpointSlice, newEndpointSlice(namespace1, service1, clusterID1, []mcsv1a1.ServicePort{port1},
				discovery.Endpoint{
					Addresses:  []string{endpointIP1},
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
				discovery.Endpoint{
					Addresses:  []string{endpointIP3},
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
				discovery.Endpoint{
					Addresses:  []string{endpointIP4},
					Conditions: discovery.EndpointConditions{Ready: &ready},
				},
			))).To(BeFalse())
		})

		It("should return DNS records with unique addresses", func() {
			t.assertDNSRecordsFound(namespace1, service1, "", "", k8snet.IPv4, true,
				endpoint1DNSRecord,
				endpoint2DNSRecord,
				endpoint3DNSRecord,
				endpoint4DNSRecord,
			)
		})
	})
}

func testHeadlessServiceInMultipleClusters() {
	t := newTestDriver()

	cluster1DNSRecord := resolver.DNSRecord{
		IP:          endpointIP1,
		Ports:       []mcsv1a1.ServicePort{port1},
		ClusterName: clusterID1,
		HostName:    endpointHostname1,
	}

	cluster2DNSRecord := resolver.DNSRecord{
		IP:          endpointIP2,
		Ports:       []mcsv1a1.ServicePort{port2},
		ClusterName: clusterID2,
		HostName:    endpointHostname2,
	}

	cluster3DNSRecord1 := resolver.DNSRecord{
		IP:          endpointIP3,
		Ports:       []mcsv1a1.ServicePort{port3, port4},
		ClusterName: clusterID3,
		HostName:    hostName1,
	}

	cluster3DNSRecord2 := resolver.DNSRecord{
		IP:          endpointIP4,
		Ports:       []mcsv1a1.ServicePort{port3, port4},
		ClusterName: clusterID3,
		HostName:    hostName1,
	}

	cluster3DNSRecord3 := resolver.DNSRecord{
		IP:          endpointIP5,
		Ports:       []mcsv1a1.ServicePort{port3, port4},
		ClusterName: clusterID3,
		HostName:    endpointHostname5,
	}

	cluster3DNSRecord4 := resolver.DNSRecord{
		IP:          endpointIP6,
		Ports:       []mcsv1a1.ServicePort{port3, port4},
		ClusterName: clusterID3,
		HostName:    hostName2,
	}

	BeforeEach(func() {
		t.resolver.PutServiceImport(newHeadlessAggregatedServiceImport(namespace1, service1))
	})

	JustBeforeEach(func() {
		t.putEndpointSlice(newEndpointSlice(namespace1, service1, clusterID1, []mcsv1a1.ServicePort{port1}, discovery.Endpoint{
			Addresses: []string{endpointIP1},
		}))

		t.putEndpointSlice(newEndpointSlice(namespace1, service1, clusterID2, []mcsv1a1.ServicePort{port2}, discovery.Endpoint{
			Addresses: []string{endpointIP2},
		}))

		t.putEndpointSlice(newEndpointSlice(namespace1, service1, clusterID3, []mcsv1a1.ServicePort{port3, port4},
			discovery.Endpoint{
				Addresses:  []string{endpointIP3, endpointIP4},
				Hostname:   &hostName1,
				NodeName:   &nodeName1,
				Conditions: discovery.EndpointConditions{Ready: &ready},
			},
			discovery.Endpoint{
				Addresses: []string{endpointIP5},
				NodeName:  &nodeName2,
			},
			discovery.Endpoint{
				Addresses: []string{endpointIP6},
				Hostname:  &hostName2,
				NodeName:  &nodeName3,
				TargetRef: &corev1.ObjectReference{
					Kind: "Pod",
					Name: "pod-host",
				},
			},
		))
	})

	Context("and no specific cluster is requested", func() {
		It("should return all the DNS records", func() {
			t.assertDNSRecordsFound(namespace1, service1, "", "", k8snet.IPv4, true,
				cluster1DNSRecord, cluster2DNSRecord, cluster3DNSRecord1, cluster3DNSRecord2, cluster3DNSRecord3, cluster3DNSRecord4)
		})
	})

	Context("and a specific cluster is requested", func() {
		It("should return all its DNS records", func() {
			t.assertDNSRecordsFound(namespace1, service1, clusterID3, "", k8snet.IPv4, true,
				cluster3DNSRecord1, cluster3DNSRecord2, cluster3DNSRecord3, cluster3DNSRecord4)
		})
	})

	Context("and a specific cluster and host name is requested", func() {
		It("should return its host name DNS records", func() {
			t.assertDNSRecordsFound(namespace1, service1, clusterID3, hostName1, k8snet.IPv4, true,
				cluster3DNSRecord1, cluster3DNSRecord2)

			t.assertDNSRecordsFound(namespace1, service1, clusterID3, hostName2, k8snet.IPv4, true,
				cluster3DNSRecord4)
		})
	})

	Context("and one becomes disconnected", func() {
		JustBeforeEach(func() {
			t.clusterStatus.DisconnectClusterID(clusterID3, k8snet.IPv4)
		})

		Context("and no specific cluster is requested", func() {
			It("should return the connected clusters' DNS records", func() {
				t.assertDNSRecordsFound(namespace1, service1, "", "", k8snet.IPv4, true,
					cluster1DNSRecord, cluster2DNSRecord)
			})
		})

		Context("and the disconnected cluster is requested", func() {
			It("should still return its DNS records", func() {
				t.assertDNSRecordsFound(namespace1, service1, clusterID3, "", k8snet.IPv4, true,
					cluster3DNSRecord1, cluster3DNSRecord2, cluster3DNSRecord3, cluster3DNSRecord4)

				t.assertDNSRecordsFound(namespace1, service1, clusterID3, hostName1, k8snet.IPv4, true,
					cluster3DNSRecord1, cluster3DNSRecord2)
			})
		})
	})

	Context("and one is subsequently removed", func() {
		JustBeforeEach(func() {
			t.resolver.RemoveEndpointSlice(newEndpointSlice(namespace1, service1, clusterID3, nil))
		})

		Context("and no specific cluster is requested", func() {
			It("should return the remaining clusters' DNS records", func() {
				t.assertDNSRecordsFound(namespace1, service1, "", "", k8snet.IPv4, true,
					cluster1DNSRecord, cluster2DNSRecord)
			})
		})

		Context("and the removed cluster is requested", func() {
			It("should return no DNS records found", func() {
				t.assertDNSRecordsNotFound(namespace1, service1, clusterID3, "")
			})
		})
	})

	Context("and the endpoints for one cluster are updated", func() {
		expDNSRecord1 := resolver.DNSRecord{
			IP:          endpointIP4,
			Ports:       []mcsv1a1.ServicePort{port3},
			ClusterName: clusterID3,
			HostName:    hostName1,
		}

		expDNSRecord2 := resolver.DNSRecord{
			IP:          endpointIP5,
			Ports:       []mcsv1a1.ServicePort{port3},
			ClusterName: clusterID3,
			HostName:    hostName2,
		}

		expDNSRecord3 := resolver.DNSRecord{
			IP:          endpointIP6,
			Ports:       []mcsv1a1.ServicePort{port3},
			ClusterName: clusterID3,
			HostName:    hostName2,
		}

		JustBeforeEach(func() {
			t.putEndpointSlice(newEndpointSlice(namespace1, service1, clusterID3, []mcsv1a1.ServicePort{port3},
				discovery.Endpoint{
					Addresses: []string{endpointIP4},
					Hostname:  &hostName1,
				},
				discovery.Endpoint{
					Addresses: []string{endpointIP5, endpointIP6},
					Hostname:  &hostName2,
				}))
		})

		It("should return the updated DNS records", func() {
			t.assertDNSRecordsFound(namespace1, service1, clusterID3, "", k8snet.IPv4, true,
				expDNSRecord1, expDNSRecord2, expDNSRecord3)

			t.assertDNSRecordsFound(namespace1, service1, clusterID3, hostName1, k8snet.IPv4, true,
				expDNSRecord1)

			t.assertDNSRecordsFound(namespace1, service1, clusterID3, hostName2, k8snet.IPv4, true,
				expDNSRecord2, expDNSRecord3)
		})
	})

	Context("and a non-existent cluster is specified", func() {
		It("should return no DNS records found", func() {
			t.assertDNSRecordsNotFound(namespace1, service1, "non-existent", "")
		})
	})
}
