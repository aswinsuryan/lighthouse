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
	"net"
	"strings"

	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
	"github.com/submariner-io/admiral/pkg/log"
	"github.com/submariner-io/lighthouse/coredns/resolver"
	"k8s.io/utils/set"
	"sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

func (lh *Lighthouse) createARecords(dnsrecords []resolver.DNSRecord, state *request.Request) []dns.RR {
	records := make([]dns.RR, 0)

	for _, record := range dnsrecords {
		dnsRecord := &dns.A{Hdr: dns.RR_Header{
			Name: state.QName(), Rrtype: dns.TypeA, Class: state.QClass(),
			Ttl: lh.TTL,
		}, A: net.ParseIP(record.IP).To4()}
		records = append(records, dnsRecord)
	}

	return records
}

func (lh *Lighthouse) createAAAARecords(dnsrecords []resolver.DNSRecord, state *request.Request) []dns.RR {
	records := make([]dns.RR, 0)

	for _, record := range dnsrecords {
		dnsRecord := &dns.AAAA{Hdr: dns.RR_Header{
			Name: state.QName(), Rrtype: dns.TypeAAAA, Class: state.QClass(),
			Ttl: lh.TTL,
		}, AAAA: net.ParseIP(record.IP).To16()}
		records = append(records, dnsRecord)
	}

	return records
}

func (lh *Lighthouse) createSRVRecords(dnsrecords []resolver.DNSRecord, state *request.Request, pReq *recordRequest, zone string,
	isHeadless bool,
) []dns.RR {
	var records []dns.RR

	for _, dnsRecord := range dnsrecords {
		var reqPorts []v1alpha1.ServicePort

		if pReq.port == "" {
			reqPorts = dnsRecord.Ports
		} else {
			logger.V(log.TRACE).Infof("Requested port %q, protocol %q for SRV", pReq.port, pReq.protocol)

			for _, port := range dnsRecord.Ports {
				name := strings.ToLower(port.Name)
				protocol := strings.ToLower(string(port.Protocol))

				logger.V(log.TRACE).Infof("Checking port %q, protocol %q", name, protocol)

				if name == pReq.port && protocol == pReq.protocol {
					reqPorts = append(reqPorts, port)
				}
			}
		}

		if len(reqPorts) == 0 {
			return nil
		}

		target := pReq.service + "." + pReq.namespace + ".svc." + zone

		if isHeadless {
			target = dnsRecord.ClusterName + "." + target
		} else if pReq.cluster != "" {
			target = pReq.cluster + "." + target
		}

		if isHeadless {
			target = dnsRecord.HostName + "." + target
		}

		portsSeen := set.New[int32]()

		for _, port := range reqPorts {
			if portsSeen.Has(port.Port) {
				continue
			}

			portsSeen.Insert(port.Port)

			record := &dns.SRV{
				Hdr:      dns.RR_Header{Name: state.QName(), Rrtype: dns.TypeSRV, Class: state.QClass(), Ttl: lh.TTL},
				Priority: 0,
				Weight:   50,
				Port:     uint16(port.Port), //nolint:gosec // Need to ignore integer conversion error
				Target:   target,
			}

			records = append(records, record)
		}
	}

	return records
}
