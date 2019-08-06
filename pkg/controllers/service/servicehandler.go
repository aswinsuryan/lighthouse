package service

import (
	"fmt"
	"net"

	lighhouseClient "github.com/submariner-io/lighthouse/pkg/apis/multiclusterservice/v1"
	lighhouseset "github.com/submariner-io/lighthouse/pkg/client/clientset/versioned"
	core_v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

// Handler interface contains the methods that are required
type Handler interface {
	Init() error
	ObjectCreated(obj interface{})
	ObjectDeleted(obj interface{})
	ObjectUpdated(objOld, objNew interface{})
}

// ServiceHandler is a sample implementation of Handler
type ServiceHandler struct {
	LighthouseClientset lighhouseset.Interface
}

// Init handles any handler initialization
func (t *ServiceHandler) Init() error {
	klog.Info("ServiceHandler.Init")
	return nil
}

// ObjectCreated is called when an object is created
func (t *ServiceHandler) ObjectCreated(obj interface{}) {
	klog.Info("ServiceHandler.ObjectCreated")
	// assert the type to a Pod object to pull out relevant data
	service := obj.(*core_v1.Service)
	multiClusterServiceInfo :=
		&[]lighhouseClient.ClusterServiceInfo{
			lighhouseClient.ClusterServiceInfo{
				ClusterID:     "abc",
				ServiceIP:     net.ParseIP(service.Spec.ClusterIP),
				ClusterDomain: "example.org",
			},
		}
	clusterInfo := &lighhouseClient.MultiClusterService{
		ObjectMeta: metav1.ObjectMeta{
			Name: service.ObjectMeta.Name,
		},
		Spec: lighhouseClient.MultiClusterServiceSpec{
			Items: *multiClusterServiceInfo,
		},
	}
	_, err := t.LighthouseClientset.LighthouseV1().MultiClusterServices("default").Create(clusterInfo)
	if err != nil {
		fmt.Println("  ERROR", err)
	}
	fmt.Println("    ClusterIp: %s", service.Spec.ClusterIP)
	fmt.Println("    ClusterPort: %s", service.Spec.Ports)
	fmt.Println("    Name: %s", service.ObjectMeta.Name)
}

// ObjectDeleted is called when an object is deleted
func (t *ServiceHandler) ObjectDeleted(obj interface{}) {
	fmt.Println("ServiceHandler.ObjectDeleted")
}

// ObjectUpdated is called when an object is updated
func (t *ServiceHandler) ObjectUpdated(objOld, objNew interface{}) {
	fmt.Println("ServiceHandler.ObjectUpdated")
}
