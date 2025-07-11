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

package framework

import (
	"context"
	"encoding/json"
	"fmt"
	goslices "slices"
	"strings"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/submariner-io/admiral/pkg/resource"
	"github.com/submariner-io/admiral/pkg/slices"
	"github.com/submariner-io/lighthouse/pkg/constants"
	"github.com/submariner-io/shipyard/test/e2e/framework"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	k8snet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
	mcsClientset "sigs.k8s.io/mcs-api/pkg/client/clientset/versioned"
)

const (
	clustersetDomain    = "clusterset.local"
	anyCount            = -1
	statefulServiceName = "nginx-ss"
	statefulSetName     = "web"
	not                 = " not"
)

var CheckedDomains = []string{clustersetDomain}

// Framework supports common operations used by e2e tests; it will keep a client & a namespace for you.
type Framework struct {
	*framework.Framework
}

var (
	MCSClients          []*mcsClientset.Clientset
	EndpointClients     []dynamic.ResourceInterface
	SubmarinerClients   []dynamic.ResourceInterface
	clusterSetIPEnabled = atomic.Pointer[bool]{}
)

func init() {
	framework.AddBeforeSuite(beforeSuite)
}

// NewFramework creates a test framework.
func NewFramework(baseName string) *Framework {
	f := &Framework{Framework: framework.NewBareFramework(baseName)}

	BeforeEach(f.BeforeEach)

	AfterEach(func() {
		namespace := f.Namespace
		f.AfterEach()

		f.AwaitEndpointSlices(framework.ClusterB, "", namespace, 0, 0)
		f.AwaitEndpointSlices(framework.ClusterA, "", namespace, 0, 0)
	})

	return f
}

func beforeSuite() {
	framework.By("Creating lighthouse clients")

	for _, restConfig := range framework.RestConfigs {
		MCSClients = append(MCSClients, createLighthouseClient(restConfig))
		EndpointClients = append(EndpointClients, createEndpointClientSet(restConfig))
		SubmarinerClients = append(SubmarinerClients, createSubmarinerClientSet(restConfig))
	}

	framework.DetectGlobalnet()
}

func createLighthouseClient(restConfig *rest.Config) *mcsClientset.Clientset {
	clientSet, err := mcsClientset.NewForConfig(restConfig)
	Expect(err).To(Not(HaveOccurred()))

	return clientSet
}

func createEndpointClientSet(restConfig *rest.Config) dynamic.ResourceInterface {
	clientSet, err := dynamic.NewForConfig(restConfig)
	Expect(err).To(Not(HaveOccurred()))

	gvr, _ := schema.ParseResourceArg("endpoints.v1.submariner.io")
	endpointsClient := clientSet.Resource(*gvr).Namespace("submariner-operator")
	_, err = endpointsClient.List(context.TODO(), metav1.ListOptions{})
	Expect(err).To(Not(HaveOccurred()))

	return endpointsClient
}

func createSubmarinerClientSet(restConfig *rest.Config) dynamic.ResourceInterface {
	clientSet, err := dynamic.NewForConfig(restConfig)
	Expect(err).To(Not(HaveOccurred()))

	gvr, _ := schema.ParseResourceArg("submariners.v1alpha1.submariner.io")
	submarinersClient := clientSet.Resource(*gvr).Namespace("submariner-operator")
	_, err = submarinersClient.List(context.TODO(), metav1.ListOptions{})
	Expect(err).To(Not(HaveOccurred()))

	return submarinersClient
}

func IsClusterSetIPEnabled() bool {
	enabledPtr := clusterSetIPEnabled.Load()
	if enabledPtr != nil {
		return *enabledPtr
	}

	gvr, _ := schema.ParseResourceArg("servicediscoveries.v1alpha1.submariner.io")
	sdClient := framework.DynClients[0].Resource(*gvr).Namespace(framework.TestContext.SubmarinerNamespace)

	framework.AwaitUntil("find service-discovery resource", func() (interface{}, error) {
		return sdClient.Get(context.TODO(), "service-discovery", metav1.GetOptions{})
	}, func(result interface{}) (bool, string, error) {
		enabled, _, err := unstructured.NestedBool(result.(*unstructured.Unstructured).Object, "spec", "clustersetIPEnabled")
		if err != nil {
			return false, "", err
		}

		clusterSetIPEnabled.Store(ptr.To(enabled))

		return true, "", nil
	})

	return *clusterSetIPEnabled.Load()
}

func (f *Framework) NewServiceExport(cluster framework.ClusterIndex, name, namespace string) *mcsv1a1.ServiceExport {
	return f.CreateServiceExport(cluster, &mcsv1a1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	})
}

func (f *Framework) CreateServiceExport(cluster framework.ClusterIndex, serviceExport *mcsv1a1.ServiceExport) *mcsv1a1.ServiceExport {
	se := MCSClients[cluster].MulticlusterV1alpha1().ServiceExports(serviceExport.Namespace)

	framework.By(fmt.Sprintf("Creating serviceExport %s.%s on %q", serviceExport.Name, serviceExport.Namespace,
		framework.TestContext.ClusterIDs[cluster]))

	return framework.AwaitUntil("create serviceExport", func() (interface{}, error) {
		return se.Create(context.TODO(), serviceExport, metav1.CreateOptions{})
	}, framework.NoopCheckResult).(*mcsv1a1.ServiceExport)
}

func (f *Framework) AwaitServiceExportedStatusCondition(cluster framework.ClusterIndex, name, namespace string) {
	framework.By(fmt.Sprintf("Retrieving ServiceExport %s.%s on %q", name, namespace, framework.TestContext.ClusterIDs[cluster]))

	se := MCSClients[cluster].MulticlusterV1alpha1().ServiceExports(namespace)

	framework.AwaitUntil("retrieve ServiceExport", func() (interface{}, error) {
		return se.Get(context.TODO(), name, metav1.GetOptions{})
	}, func(result interface{}) (bool, string, error) {
		se := result.(*mcsv1a1.ServiceExport)

		for i := range se.Status.Conditions {
			if se.Status.Conditions[i].Type == constants.ServiceExportReady {
				if se.Status.Conditions[i].Status != metav1.ConditionTrue {
					out, _ := json.MarshalIndent(se.Status.Conditions[i], "", "  ")
					return false, fmt.Sprintf("ServiceExport %s condition status is %s", constants.ServiceExportReady, out), nil
				}

				return true, "", nil
			}
		}

		out, _ := json.MarshalIndent(se.Status.Conditions, "", " ")

		return false, fmt.Sprintf("ServiceExport %s condition status not found. Actual: %s",
			constants.ServiceExportReady, out), nil
	})
}

func (f *Framework) DeleteServiceExport(cluster framework.ClusterIndex, name, namespace string) {
	framework.By(fmt.Sprintf("Deleting serviceExport %s.%s on %q", name, namespace, framework.TestContext.ClusterIDs[cluster]))
	framework.AwaitUntil("delete service export", func() (interface{}, error) {
		return nil, MCSClients[cluster].MulticlusterV1alpha1().ServiceExports(namespace).Delete(
			context.TODO(), name, metav1.DeleteOptions{})
	}, framework.NoopCheckResult)
}

func (f *Framework) GetService(cluster framework.ClusterIndex, name, namespace string) (*v1.Service, error) {
	framework.By(fmt.Sprintf("Retrieving service %s.%s on %q", name, namespace, framework.TestContext.ClusterIDs[cluster]))
	return framework.KubeClients[cluster].CoreV1().Services(namespace).Get(context.TODO(), name, metav1.GetOptions{})
}

func (f *Framework) AwaitAggregatedServiceImport(targetCluster framework.ClusterIndex, svc *v1.Service, clusterCount int,
) *mcsv1a1.ServiceImport {
	framework.By(fmt.Sprintf("Retrieving ServiceImport for %q in ns %q on %q", svc.Name, svc.Namespace,
		framework.TestContext.ClusterIDs[targetCluster]))

	var si *mcsv1a1.ServiceImport

	siClient := MCSClients[targetCluster].MulticlusterV1alpha1().ServiceImports(svc.Namespace)

	framework.AwaitUntil("retrieve ServiceImport", func() (interface{}, error) {
		obj, err := siClient.Get(context.TODO(), svc.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil //nolint:nilnil // Intentional
		}

		return obj, err
	}, func(result interface{}) (bool, string, error) {
		if clusterCount == 0 {
			if result != nil {
				return false, "ServiceImport still exists", nil
			}

			return true, "", nil
		}

		if result == nil {
			return false, "ServiceImport not found", nil
		}

		si = result.(*mcsv1a1.ServiceImport)

		if len(si.Status.Clusters) != clusterCount {
			return false, fmt.Sprintf("Actual cluster count %d does not match expected %d",
				len(si.Status.Clusters), clusterCount), nil
		}

		expPorts := make([]mcsv1a1.ServicePort, len(svc.Spec.Ports))
		for i := range svc.Spec.Ports {
			expPorts[i] = mcsv1a1.ServicePort{
				Name:     svc.Spec.Ports[i].Name,
				Protocol: svc.Spec.Ports[i].Protocol,
				Port:     svc.Spec.Ports[i].Port,
			}
		}

		if svc.Spec.ClusterIP != v1.ClusterIPNone && !slices.Equivalent(expPorts, si.Spec.Ports, func(p mcsv1a1.ServicePort) string {
			return fmt.Sprintf("%s%s%d", p.Name, p.Protocol, p.Port)
		}) {
			return false, fmt.Sprintf("ServiceImport ports do not match. Expected: %s, Actual: %s",
				resource.ToJSON(expPorts), resource.ToJSON(si.Spec.Ports)), nil
		}

		return true, "", nil
	})

	return si
}

func (f *Framework) NewHeadlessServiceWithParams(name, portName string, protcol v1.Protocol,
	labelsMap map[string]string, cluster framework.ClusterIndex, ipFamilyPolicy *v1.IPFamilyPolicy,
) *v1.Service {
	var port int32 = 80

	nginxService := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.ServiceSpec{
			Type:           v1.ServiceTypeClusterIP,
			ClusterIP:      v1.ClusterIPNone,
			IPFamilyPolicy: ipFamilyPolicy,
			Ports: []v1.ServicePort{
				{
					Port:     port,
					Name:     portName,
					Protocol: protcol,
				},
			},
		},
	}

	if len(labelsMap) != 0 {
		nginxService.Labels = labelsMap
		nginxService.Spec.Selector = labelsMap
	}

	sc := framework.KubeClients[cluster].CoreV1().Services(f.Namespace)
	service := framework.AwaitUntil("create service", func() (interface{}, error) {
		return sc.Create(context.TODO(), &nginxService, metav1.CreateOptions{})
	}, framework.NoopCheckResult).(*v1.Service)

	return service
}

func (f *Framework) NewNginxHeadlessService(cluster framework.ClusterIndex) *v1.Service {
	return f.NewHeadlessServiceWithParams("nginx-headless", "http", v1.ProtocolTCP,
		map[string]string{"app": "nginx-demo"}, cluster, nil)
}

func (f *Framework) NewHeadlessServiceEndpointIP(cluster framework.ClusterIndex) *v1.Service {
	return f.NewHeadlessServiceWithParams("ep-headless", "http", v1.ProtocolTCP, map[string]string{}, cluster, nil)
}

func (f *Framework) NewEndpointForHeadlessService(cluster framework.ClusterIndex, svc *v1.Service) {
	ep := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name: svc.Name,
		},
		Subsets: []v1.EndpointSubset{
			{
				Addresses: []v1.EndpointAddress{
					{
						IP:       "192.168.5.1",
						Hostname: "host1",
					},
					{
						IP:       "192.168.5.2",
						Hostname: "host2",
					},
				},
			},
		},
	}

	for i := range svc.Spec.Ports {
		ep.Subsets[0].Ports = append(ep.Subsets[0].Ports, v1.EndpointPort{
			Name:     svc.Spec.Ports[i].Name,
			Port:     svc.Spec.Ports[i].Port,
			Protocol: svc.Spec.Ports[i].Protocol,
		})
	}

	ec := framework.KubeClients[cluster].CoreV1().Endpoints(svc.Namespace)
	framework.AwaitUntil("create endpoint", func() (interface{}, error) {
		return ec.Create(context.TODO(), ep, metav1.CreateOptions{})
	}, framework.NoopCheckResult)
}

func addrToHostname(addr string) string {
	return strings.ReplaceAll(strings.ReplaceAll(addr, ":", "-"), ".", "-")
}

func (f *Framework) AwaitEndpointIPs(targetCluster framework.ClusterIndex, name, namespace string, count int,
	addrType discovery.AddressType,
) ([]string, []string) {
	var ipList, hostNameList []string

	client := framework.KubeClients[targetCluster].DiscoveryV1().EndpointSlices(namespace)
	framework.By(fmt.Sprintf("Retrieving Endpoints for %s on %q", name, framework.TestContext.ClusterIDs[targetCluster]))
	framework.AwaitUntil("retrieve Endpoints", func() (interface{}, error) {
		return client.List(context.Background(), metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(map[string]string{
				discovery.LabelServiceName: name,
			}).String(),
		})
	}, func(result interface{}) (bool, string, error) {
		ipList = make([]string, 0)
		hostNameList = make([]string, 0)

		epsList := result.(*discovery.EndpointSliceList).Items
		for i := range epsList {
			if epsList[i].AddressType != addrType {
				continue
			}

			for j := range epsList[i].Endpoints {
				ep := &epsList[i].Endpoints[j]
				if !ptr.Deref(ep.Conditions.Ready, true) {
					continue
				}

				for _, addr := range ep.Addresses {
					ipList = append(ipList, addr)

					if ptr.Deref(ep.Hostname, "") != "" {
						hostNameList = append(hostNameList, *ep.Hostname)
					} else {
						hostNameList = append(hostNameList, addrToHostname(addr))
					}
				}
			}
		}

		if count != anyCount && len(ipList) != count {
			return false, fmt.Sprintf("endpoints have %d IPs when expected %d", len(ipList), count), nil
		}

		return true, "", nil
	})

	return ipList, hostNameList
}

func (f *Framework) AwaitPodIngressIPs(targetCluster framework.ClusterIndex, svc *v1.Service, count int,
	isLocal bool,
) ([]string, []string) {
	podList := f.Framework.AwaitPodsByAppLabel(targetCluster, svc.Labels["app"], svc.Namespace, count)
	hostNameList := make([]string, 0)
	ipList := make([]string, 0)

	for i := range len(podList.Items) {
		ingressIPName := "pod-" + podList.Items[i].Name
		ingressIP := f.Framework.AwaitGlobalIngressIP(targetCluster, ingressIPName, svc.Namespace)

		ip := ingressIP
		if isLocal {
			ip = podList.Items[i].Status.PodIP
		}

		ipList = append(ipList, ip)

		hostname := podList.Items[i].Spec.Hostname
		if hostname == "" {
			hostname = addrToHostname(ip)
		}

		hostNameList = append(hostNameList, hostname)
	}

	return ipList, hostNameList
}

func (f *Framework) AwaitPodIPs(targetCluster framework.ClusterIndex, svc *v1.Service, count int,
	isLocal bool,
) ([]string, []string) {
	if framework.TestContext.GlobalnetEnabled {
		return f.AwaitPodIngressIPs(targetCluster, svc, count, isLocal)
	}

	return f.AwaitEndpointIPs(targetCluster, svc.Name, svc.Namespace, count, discovery.AddressTypeIPv4)
}

func (f *Framework) GetPodIPs(targetCluster framework.ClusterIndex, service *v1.Service, isLocal bool) ([]string, []string) {
	return f.AwaitPodIPs(targetCluster, service, 1, isLocal)
}

func (f *Framework) AwaitEndpointIngressIPs(targetCluster framework.ClusterIndex, svc *v1.Service) ([]string, []string) {
	hostNameList := make([]string, 0)
	ipList := make([]string, 0)

	endpoint := framework.AwaitUntil("retrieve Endpoints", func() (interface{}, error) {
		return framework.KubeClients[targetCluster].CoreV1().Endpoints(svc.Namespace).Get(context.TODO(), svc.Name, metav1.GetOptions{})
	}, framework.NoopCheckResult).(*v1.Endpoints)

	for _, subset := range endpoint.Subsets {
		for _, address := range subset.Addresses {
			// Add hostname to hostNameList
			if address.Hostname != "" {
				hostNameList = append(hostNameList, address.Hostname)
			}

			// Add globalIP to ipList
			gip := f.Framework.AwaitGlobalIngressIP(targetCluster, fmt.Sprintf("ep-%.44s-%.15s", endpoint.Name, address.IP), svc.Namespace)
			ipList = append(ipList, gip)
		}
	}

	return ipList, hostNameList
}

func (f *Framework) GetEndpointIPs(targetCluster framework.ClusterIndex, svc *v1.Service) ([]string, []string) {
	if framework.TestContext.GlobalnetEnabled {
		return f.AwaitEndpointIngressIPs(targetCluster, svc)
	}

	return f.AwaitEndpointIPs(targetCluster, svc.Name, svc.Namespace, anyCount, discovery.AddressTypeIPv4)
}

func (f *Framework) SetNginxReplicaSet(cluster framework.ClusterIndex, count uint32) *appsv1.Deployment {
	framework.By(fmt.Sprintf("Setting Nginx deployment replicas to %v in cluster %q", count, framework.TestContext.ClusterIDs[cluster]))
	patch := fmt.Sprintf(`{"spec":{"replicas":%v}}`, count)
	deployments := framework.KubeClients[cluster].AppsV1().Deployments(f.Namespace)
	result := framework.AwaitUntil("set replicas", func() (interface{}, error) {
		return deployments.Patch(context.TODO(), "nginx-demo", types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	}, framework.NoopCheckResult).(*appsv1.Deployment)

	return result
}

func (f *Framework) NewNginxStatefulSet(cluster framework.ClusterIndex) *appsv1.StatefulSet {
	var replicaCount int32 = 1
	var port int32 = 8080

	nginxStatefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: statefulSetName,
		},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": statefulServiceName,
				},
			},
			ServiceName: statefulServiceName,
			Replicas:    &replicaCount,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": statefulServiceName,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:            statefulServiceName,
							Image:           framework.TestContext.NettestImageURL,
							ImagePullPolicy: v1.PullAlways,
							Ports: []v1.ContainerPort{
								{
									ContainerPort: port,
									Name:          "web",
								},
							},
							Command: []string{"/app/simpleserver"},
						},
					},
					RestartPolicy: v1.RestartPolicyAlways,
				},
			},
		},
	}

	create(f, cluster, nginxStatefulSet)

	return nginxStatefulSet
}

func create(f *Framework, cluster framework.ClusterIndex, statefulSet *appsv1.StatefulSet) *v1.PodList {
	count := statefulSet.Spec.Replicas
	pc := framework.KubeClients[cluster].AppsV1().StatefulSets(f.Namespace)
	appName := statefulSet.Spec.Template.ObjectMeta.Labels["app"]

	_ = framework.AwaitUntil("create statefulSet", func() (interface{}, error) {
		return pc.Create(context.TODO(), statefulSet, metav1.CreateOptions{})
	}, framework.NoopCheckResult).(*appsv1.StatefulSet)

	return f.AwaitPodsByAppLabel(cluster, appName, f.Namespace, int(*count))
}

func (f *Framework) AwaitEndpointSlices(targetCluster framework.ClusterIndex, name, namespace string,
	expSliceCount, expReadyCount int,
) *discovery.EndpointSliceList {
	var endpointSliceList *discovery.EndpointSliceList

	ep := framework.KubeClients[targetCluster].DiscoveryV1().EndpointSlices(namespace)
	labelMap := map[string]string{
		discovery.LabelManagedBy: constants.LabelValueManagedBy,
	}
	listOptions := metav1.ListOptions{
		LabelSelector: labels.Set(labelMap).String(),
	}

	framework.By(fmt.Sprintf("Retrieving EndpointSlices for %q in ns %q on %q", name, namespace,
		framework.TestContext.ClusterIDs[targetCluster]))
	framework.AwaitUntil("retrieve EndpointSlices", func() (interface{}, error) {
		return ep.List(context.TODO(), listOptions)
	}, func(result interface{}) (bool, string, error) {
		endpointSliceList = result.(*discovery.EndpointSliceList)
		sliceCount := 0
		readyCount := 0

		for i := range endpointSliceList.Items {
			es := &endpointSliceList.Items[i]
			if name == "" || es.Labels[mcsv1a1.LabelServiceName] == name {
				sliceCount++

				for j := range es.Endpoints {
					if es.Endpoints[j].Conditions.Ready == nil || *es.Endpoints[j].Conditions.Ready {
						readyCount++
					}
				}
			}
		}

		if expSliceCount != anyCount && sliceCount != expSliceCount {
			return false, fmt.Sprintf("%d EndpointSlices found when expected %d", sliceCount, expSliceCount), nil
		}

		if expReadyCount != anyCount && readyCount != expReadyCount {
			return false, fmt.Sprintf("%d ready Endpoints found when expected %d", readyCount, expReadyCount), nil
		}

		return true, "", nil
	})

	return endpointSliceList
}

func (f *Framework) SetNginxStatefulSetReplicas(cluster framework.ClusterIndex, count uint32) *appsv1.StatefulSet {
	framework.By(fmt.Sprintf("Setting Nginx statefulset replicas to %v", count))
	patch := fmt.Sprintf(`{"spec":{"replicas":%v}}`, count)
	ss := framework.KubeClients[cluster].AppsV1().StatefulSets(f.Namespace)
	result := framework.AwaitUntil("set replicas", func() (interface{}, error) {
		return ss.Patch(context.TODO(), statefulSetName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	}, framework.NoopCheckResult).(*appsv1.StatefulSet)

	return result
}

func (f *Framework) GetHealthCheckIPInfo(cluster framework.ClusterIndex) (string, string) {
	var endpointName, healthCheckIP string

	framework.AwaitUntil("Get healthCheckIP", func() (interface{}, error) {
		unstructuredEndpointList, err := EndpointClients[cluster].List(context.TODO(), metav1.ListOptions{})
		return unstructuredEndpointList, err
	}, func(result interface{}) (bool, string, error) {
		unstructuredEndpointList := result.(*unstructured.UnstructuredList)
		for _, endpoint := range unstructuredEndpointList.Items {
			framework.By(fmt.Sprintf("Getting the endpoint %s, for cluster %s", endpoint.GetName(), framework.TestContext.ClusterIDs[cluster]))

			if strings.Contains(endpoint.GetName(), framework.TestContext.ClusterIDs[cluster]) {
				endpointName = endpoint.GetName()

				var found bool
				var err error

				healthCheckIP, found, err = unstructured.NestedString(endpoint.Object, "spec", "healthCheckIP")
				if err != nil {
					return false, "", err
				}

				if !found {
					return false, fmt.Sprintf("HealthcheckIP not found in %#v ", endpoint), nil
				}
			}
		}

		return true, "", nil
	})

	return endpointName, healthCheckIP
}

func (f *Framework) GetHealthCheckEnabledInfo(cluster framework.ClusterIndex) bool {
	var healthCheckEnabled bool

	framework.AwaitUntil("Get healthCheckEnabled Configuration", func() (interface{}, error) {
		unstructuredSubmarinerConfig, err := SubmarinerClients[cluster].Get(context.TODO(),
			"submariner", metav1.GetOptions{})
		return unstructuredSubmarinerConfig, err
	}, func(result interface{}) (bool, string, error) {
		unstructuredSubmarinerConfig := result.(*unstructured.Unstructured)

		framework.By("Getting the Submariner Config, for cluster " + framework.TestContext.ClusterIDs[cluster])

		var found bool
		var err error

		healthCheckEnabled, found, err = unstructured.NestedBool(unstructuredSubmarinerConfig.Object,
			"spec", "connectionHealthCheck", "enabled")
		if err != nil {
			return false, "", err
		}

		return found, "", nil
	})

	return healthCheckEnabled
}

func (f *Framework) SetHealthCheckIP(cluster framework.ClusterIndex, ip, endpointName string) {
	framework.By(fmt.Sprintf("Setting health check IP cluster %q to %v", framework.TestContext.ClusterIDs[cluster], ip))
	patch := fmt.Sprintf(`{"spec":{"healthCheckIP":%q}}`, ip)

	framework.AwaitUntil("set healthCheckIP", func() (interface{}, error) {
		endpoint, err := EndpointClients[cluster].Patch(context.TODO(), endpointName, types.MergePatchType, []byte(patch),
			metav1.PatchOptions{})
		return endpoint, err
	}, framework.NoopCheckResult)
}

func (f *Framework) VerifyServiceIPWithDig(srcCluster, targetCluster framework.ClusterIndex, service *v1.Service, targetPod *v1.PodList,
	domains []string, clusterName string, shouldContain bool,
) {
	serviceIP := f.GetServiceIP(targetCluster, service, v1.IPFamilyUnknown)
	f.VerifyIPWithDig(srcCluster, service, targetPod, domains, clusterName, serviceIP, shouldContain)
}

func BuildServiceDNSName(clusterName, serviceName, namespace, tld string) string {
	name := ""

	if clusterName != "" {
		name = clusterName + "."
	}

	return name + serviceName + "." + namespace + ".svc." + tld
}

func (f *Framework) VerifyIPWithDig(srcCluster framework.ClusterIndex, service *v1.Service, targetPod *v1.PodList,
	domains []string, clusterName, serviceIP string, shouldContain bool,
) {
	cmd := []string{"dig", "+short"}

	if k8snet.IPFamilyOfString(serviceIP) == k8snet.IPv6 {
		cmd = append(cmd, "AAAA")
	}

	for i := range domains {
		cmd = append(cmd, BuildServiceDNSName(clusterName, service.Name, f.Namespace, domains[i]))
	}

	op := "is"
	if !shouldContain {
		op += not
	}

	framework.By(fmt.Sprintf("Executing %q to verify IP %q for service %q %q discoverable", strings.Join(cmd, " "), serviceIP,
		service.Name, op))
	framework.AwaitUntil("verify if service IP is discoverable", func() (interface{}, error) {
		stdout, _, err := f.ExecWithOptions(context.TODO(), &framework.ExecOptions{
			Command:       cmd,
			Namespace:     targetPod.Items[0].Namespace,
			PodName:       targetPod.Items[0].Name,
			ContainerName: targetPod.Items[0].Spec.Containers[0].Name,
			CaptureStdout: true,
			CaptureStderr: true,
		}, srcCluster)
		if err != nil {
			return nil, err
		}

		return stdout, nil
	}, func(result interface{}) (bool, string, error) {
		doesContain := strings.Contains(result.(string), serviceIP)
		framework.By(fmt.Sprintf("Validating that dig result %q %s %q", result, op, serviceIP))

		if doesContain && !shouldContain {
			return false, fmt.Sprintf("expected execution result %q not to contain %q", result, serviceIP), nil
		}

		if !doesContain && shouldContain {
			return false, fmt.Sprintf("expected execution result %q to contain %q", result, serviceIP), nil
		}

		return true, "", nil
	})
}

func (f *Framework) VerifyIPsWithDig(cluster framework.ClusterIndex, service *v1.Service, targetPod *v1.PodList,
	ipList, domains []string, clusterName string, shouldContain bool,
) {
	f.VerifyIPsWithDigByFamily(cluster, service, targetPod, ipList, domains, clusterName, shouldContain, k8snet.IPv4)
}

func (f *Framework) VerifyIPsWithDigByFamily(cluster framework.ClusterIndex, service *v1.Service, targetPod *v1.PodList,
	ipList, domains []string, clusterName string, shouldContain bool, ipFamily k8snet.IPFamily,
) {
	cmd := []string{"dig", "+short"}

	if ipFamily == k8snet.IPv6 {
		cmd = append(cmd, "AAAA")
	}

	var clusterDNSName string
	if clusterName != "" {
		clusterDNSName = clusterName + "."
	}

	for i := range domains {
		cmd = append(cmd, clusterDNSName+service.Name+"."+f.Namespace+".svc."+domains[i])
	}

	op := "are"
	if !shouldContain {
		op += not
	}

	framework.By(fmt.Sprintf("Executing %q to verify IPs %v for service %q %q discoverable", strings.Join(cmd, " "), ipList, service.Name, op))
	framework.AwaitUntil(" service IP verification", func() (interface{}, error) {
		stdout, _, err := f.ExecWithOptions(context.TODO(), &framework.ExecOptions{
			Command:       cmd,
			Namespace:     f.Namespace,
			PodName:       targetPod.Items[0].Name,
			ContainerName: targetPod.Items[0].Spec.Containers[0].Name,
			CaptureStdout: true,
			CaptureStderr: true,
		}, cluster)
		if err != nil {
			return nil, err
		}

		return stdout, nil
	}, func(result interface{}) (bool, string, error) {
		framework.By(fmt.Sprintf("Validating that dig result %s %q", op, result))

		if len(ipList) == 0 && result != "" {
			return false, fmt.Sprintf("expected execution result %q to be empty", result), nil
		}

		for _, ip := range ipList {
			count := strings.Count(result.(string), ip)
			if count > 0 && !shouldContain {
				return false, fmt.Sprintf("expected execution result %q not to contain %q", result, ip), nil
			}

			if count != 1 && shouldContain {
				return false, fmt.Sprintf("expected execution result %q to contain one occurrence of %q", result, ip), nil
			}
		}

		return true, "", nil
	})
}

func (f *Framework) GetServiceIP(svcCluster framework.ClusterIndex, service *v1.Service, ipFamily v1.IPFamily) string {
	Expect(service.Spec.Type).To(Equal(v1.ServiceTypeClusterIP))

	var serviceIP string

	if ipFamily == v1.IPFamilyUnknown {
		Expect(ptr.Deref(service.Spec.IPFamilyPolicy, v1.IPFamilyPolicySingleStack)).To(Equal(v1.IPFamilyPolicySingleStack))
		serviceIP = service.Spec.ClusterIP
	} else {
		index := goslices.Index(service.Spec.IPFamilies, ipFamily)
		Expect(index).To(BeNumerically(">=", 0))

		serviceIP = service.Spec.ClusterIPs[index]
	}

	if framework.TestContext.GlobalnetEnabled && k8snet.IPFamilyOfString(serviceIP) == k8snet.IPv4 {
		serviceIP = f.Framework.AwaitGlobalIngressIP(svcCluster, service.Name, service.Namespace)
	}

	return serviceIP
}
