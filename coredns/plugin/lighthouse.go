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
	"errors"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/submariner-io/admiral/pkg/log"
	"github.com/submariner-io/lighthouse/coredns/resolver"
	k8snet "k8s.io/utils/net"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	Svc        = "svc"
	Pod        = "pod"
	defaultTTL = uint32(5)
)

var errInvalidRequest = errors.New("invalid query name")

var logger = log.Logger{Logger: logf.Log.WithName("Handler")}

type Lighthouse struct {
	Next                plugin.Handler
	Fall                fall.F
	Zones               []string
	TTL                 uint32
	ClusterStatus       resolver.ClusterStatus
	Resolver            *resolver.Interface
	SupportedIPFamilies []k8snet.IPFamily
}

var _ plugin.Handler = &Lighthouse{}
