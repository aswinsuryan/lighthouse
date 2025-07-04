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

package lighthouse

import (
	"context"
	"errors"
	"os"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/miekg/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/submariner-io/admiral/pkg/syncer/test"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	fakeClient "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	k8snet "k8s.io/utils/net"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

type fakeHandler struct{}

func (f *fakeHandler) ServeDNS(_ context.Context, _ dns.ResponseWriter, _ *dns.Msg) (int, error) {
	return dns.RcodeSuccess, nil
}

func (f *fakeHandler) Name() string {
	return "fake"
}

var _ = Describe("Plugin setup", func() {
	BeforeEach(func() {
		os.Unsetenv("SUBMARINER_CLUSTERCIDR")

		gatewaysGVR := schema.GroupVersionResource{
			Group:    "submariner.io",
			Version:  "v1",
			Resource: "gateways",
		}

		submarinersGVR := schema.GroupVersionResource{
			Group:    "submariner.io",
			Version:  "v1alpha1",
			Resource: "submariners",
		}

		newDynamicClient = func(_ *rest.Config) (dynamic.Interface, error) {
			return fakeClient.NewSimpleDynamicClientWithCustomListKinds(scheme.Scheme, map[schema.GroupVersionResource]string{
				gatewaysGVR:    "GatewayList",
				submarinersGVR: "SubmarinersList",
			}), nil
		}

		restMapper = test.GetRESTMapperFor(&discovery.EndpointSlice{}, &mcsv1a1.ServiceImport{})
	})

	AfterEach(func() {
		newDynamicClient = nil
	})

	Context("Parsing correct configurations", testCorrectConfig)
	Context("Parsing incorrect configurations", testIncorrectConfig)
	Context("Plugin registration", testPluginRegistration)
})

func testCorrectConfig() {
	var (
		lh     *Lighthouse
		config string
	)

	BeforeEach(func() {
		buildKubeConfigFunc = func(_, _ string) (*rest.Config, error) {
			return &rest.Config{}, nil
		}

		config = PluginName
	})

	JustBeforeEach(func() {
		var err error
		lh, err = lighthouseParse(caddy.NewTestController("dns", config))
		Expect(err).To(Succeed())
	})

	When("no optional arguments are specified", func() {
		It("should succeed with empty zones and fallthrough fields", func() {
			Expect(lh.Fall).To(Equal(fall.F{}))
			Expect(lh.Zones).To(BeEmpty())
		})
	})

	When("lighthouse zone and fallthrough zone arguments are specified", func() {
		BeforeEach(func() {
			config = `lighthouse cluster2.local cluster3.local {
			    fallthrough cluster2.local
            }`
		})

		It("should succeed with the zones and fallthrough fields populated correctly", func() {
			Expect(lh.Fall).To(Equal(fall.F{Zones: []string{"cluster2.local."}}))
			Expect(lh.Zones).To(Equal([]string{"cluster2.local.", "cluster3.local."}))
		})
	})

	When("fallthrough argument with no zones is specified", func() {
		BeforeEach(func() {
			config = `lighthouse {
			    fallthrough
            }`
		})

		It("should succeed with the root fallthrough zones", func() {
			Expect(lh.Fall).Should(Equal(fall.Root))
			Expect(lh.Zones).Should(BeEmpty())
		})
	})

	When("ttl arguments is specified", func() {
		BeforeEach(func() {
			config = `lighthouse {
			    ttl 30
            }`
		})

		It("should succeed with the ttl field populated correctly", func() {
			Expect(lh.TTL).Should(Equal(uint32(30)))
		})
	})

	It("Should handle missing optional fields", func() {
		config := `lighthouse`
		c := caddy.NewTestController("dns", config)
		lh, err := lighthouseParse(c)
		Expect(err).NotTo(HaveOccurred())

		setupErr := setupLighthouse(c) // For coverage
		Expect(setupErr).NotTo(HaveOccurred())
		Expect(lh.Fall).Should(Equal(fall.F{}))
		Expect(lh.Zones).Should(BeEmpty())
		Expect(lh.TTL).Should(Equal(defaultTTL))
	})

	When("no cluster CIDRs are specified", func() {
		It("should default to supported address type IPv4", func() {
			Expect(lh.SupportedIPFamilies).To(Equal([]k8snet.IPFamily{k8snet.IPv4}))
		})
	})

	When("an IPv6 cluster CIDR is specified", func() {
		BeforeEach(func() {
			os.Setenv("SUBMARINER_CLUSTERCIDR", "2001:0:0:1234::/64")
		})

		It("should set the supported address type IPv6", func() {
			Expect(lh.SupportedIPFamilies).To(Equal([]k8snet.IPFamily{k8snet.IPv6}))
		})
	})

	When("dual-stack cluster CIDRs are specified", func() {
		BeforeEach(func() {
			os.Setenv("SUBMARINER_CLUSTERCIDR", "2001:0:0:1234::/64, 10.253.1.0/16")
		})

		It("should set the supported address types to IPv4 and IPv6", func() {
			Expect(lh.SupportedIPFamilies).To(ContainElements(k8snet.IPv4, k8snet.IPv6))
		})
	})
}

func testIncorrectConfig() {
	var (
		setupErr error
		config   string
	)

	JustBeforeEach(func() {
		setupErr = setupLighthouse(caddy.NewTestController("dns", config))
	})

	When("an unexpected argument is specified", func() {
		BeforeEach(func() {
			config = `lighthouse {
                dummy
		    } noplugin`

			buildKubeConfigFunc = func(_, _ string) (*rest.Config, error) {
				return &rest.Config{}, nil
			}
		})

		It("should return an appropriate plugin error", func() {
			verifyPluginError(setupErr, "dummy")
		})
	})

	When("an empty ttl is specified", func() {
		BeforeEach(func() {
			config = `lighthouse {
                ttl
		    } noplugin`

			buildKubeConfigFunc = func(_, _ string) (*rest.Config, error) {
				return &rest.Config{}, nil
			}
		})

		It("should return an appropriate plugin error", func() {
			verifyPluginError(setupErr, "Wrong argument count")
		})
	})

	When("an invalid ttl is specified", func() {
		BeforeEach(func() {
			config = `lighthouse {
                ttl -10
		    } noplugin`

			buildKubeConfigFunc = func(_, _ string) (*rest.Config, error) {
				return &rest.Config{}, nil
			}
		})

		It("should return an appropriate plugin error", func() {
			verifyPluginError(setupErr, "ttl must be in range [0, 3600]: -10")
		})
	})

	When("building the kubeconfig fails", func() {
		BeforeEach(func() {
			config = PluginName

			buildKubeConfigFunc = func(_, _ string) (*rest.Config, error) {
				return nil, errors.New("mock")
			}
		})

		It("should return an appropriate plugin error", func() {
			verifyPluginError(setupErr, "mock")
		})
	})
}

func testPluginRegistration() {
	When("plugin setup succeeds", func() {
		It("should properly register the Lighthouse plugin with the DNS server", func() {
			buildKubeConfigFunc = func(_, _ string) (*rest.Config, error) {
				return &rest.Config{}, nil
			}

			controller := caddy.NewTestController("dns", PluginName)
			err := setupLighthouse(controller)
			Expect(err).To(Succeed())

			plugins := dnsserver.GetConfig(controller).Plugin
			Expect(plugins).To(HaveLen(1))

			fakeHandler := &fakeHandler{}
			handler := plugins[0](fakeHandler)
			lh, ok := handler.(*Lighthouse)
			Expect(ok).To(BeTrue(), "Unexpected Handler type %T", handler)
			Expect(lh.Next).To(BeIdenticalTo(fakeHandler))
		})
	})
}

func verifyPluginError(err error, str string) {
	Expect(err).To(HaveOccurred())
	Expect(err.Error()).To(HavePrefix("plugin/lighthouse"))
	Expect(err.Error()).To(ContainSubstring(str))
}
