// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client_test

import (
	"encoding/base64"
	"fmt"
	"net"
	"testing"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	hcm "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/envoyproxy/go-control-plane/pkg/cache"
	xds "github.com/envoyproxy/go-control-plane/pkg/server"
	"github.com/envoyproxy/go-control-plane/pkg/util"
	"google.golang.org/grpc"

	meshconfig "istio.io/api/mesh/v1alpha1"
	mixerpb "istio.io/api/mixer/v1"
	"istio.io/istio/mixer/test/client/env"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pilot/pkg/networking/plugin/mixer"
	pilotutil "istio.io/istio/pilot/pkg/networking/util"
)

const (
	envoyConf = `
admin:
  access_log_path: {{.AccessLogPath}}
  address:
    socket_address:
      address: 127.0.0.1
      port_value: {{.Ports.AdminPort}}
node:
  id: id
  cluster: unknown
  metadata:
    # these two must come together and they need to be set
    NODE_UID: pod.ns
    NODE_NAMESPACE: ns
dynamic_resources:
  lds_config: { ads: {} }
  ads_config:
    api_type: GRPC
    grpc_services:
      envoy_grpc:
        cluster_name: xds
static_resources:
  clusters:
  - name: xds
    http2_protocol_options: {}
    connect_timeout: 5s
    type: STATIC
    hosts:
    - socket_address:
        address: 127.0.0.1
        port_value: {{.Ports.DiscoveryPort}}
  - name: "outbound|||svc.ns3"
    connect_timeout: 5s
    type: STATIC
    hosts:
    - socket_address:
        address: 127.0.0.1
        port_value: {{.Ports.BackendPort}}
  - name: "outbound|9091||mixer_server"
    http2_protocol_options: {}
    connect_timeout: 5s
    type: STATIC
    hosts:
    - socket_address:
        address: 127.0.0.1
        port_value: {{.Ports.MixerPort}}
`

	checkAttributesOkOutbound = `
{
  "connection.mtls": false,
  "origin.ip": "[127 0 0 1]",
  "context.protocol": "http",
  "context.reporter.kind": "outbound",
  "context.reporter.uid": "kubernetes://pod.ns",
  "destination.service.host": "svc.ns3",
  "destination.service.name": "svc",
  "destination.service.namespace": "ns3",
  "destination.service.uid": "istio://ns3/services/svc",
  "source.uid": "%s",
  "source.namespace": "ns",
  "request.headers": {
     ":method": "GET",
     ":path": "/echo",
     ":authority": "*",
     "x-forwarded-proto": "http",
     "x-request-id": "*"
  },
  "request.host": "*",
  "request.path": "/echo",
  "request.time": "*",
  "request.useragent": "Go-http-client/1.1",
  "request.method": "GET",
  "request.scheme": "http",
  "request.url_path": "/echo"
}
`
	reportAttributesOkOutbound = `
{
  "connection.mtls": false,
  "origin.ip": "[127 0 0 1]",
  "context.protocol": "http",
  "context.proxy_error_code": "-",
  "context.reporter.kind": "outbound",
  "context.reporter.uid": "kubernetes://pod.ns",
  "destination.ip": "[127 0 0 1]",
  "destination.port": "*",
  "destination.service.host": "svc.ns3",
  "destination.service.name": "svc",
  "destination.service.namespace": "ns3",
  "destination.service.uid": "istio://ns3/services/svc",
  "source.uid": "%s",
  "source.namespace": "ns",
  "check.cache_hit": false,
  "quota.cache_hit": false,
  "request.headers": {
     ":method": "GET",
     ":path": "/echo",
     ":authority": "*",
     "x-forwarded-proto": "http",
     "x-istio-attributes": "-",
     "x-request-id": "*"
  },
  "request.host": "*",
  "request.path": "/echo",
  "request.time": "*",
  "request.useragent": "Go-http-client/1.1",
  "request.method": "GET",
  "request.scheme": "http",
  "request.size": 0,
  "request.total_size": "*",
  "request.url_path": "/echo",
  "response.time": "*",
  "response.size": 0,
  "response.duration": "*",
  "response.code": 200,
  "response.headers": {
     "date": "*",
     "content-length": "0",
     ":status": "200",
     "server": "envoy"
  },
  "response.total_size": "*"
}`
)

func TestGateway(t *testing.T) {
	s := env.NewTestSetup(env.GatewayTest, t)
	s.EnvoyTemplate = envoyConf
	grpcServer := grpc.NewServer()
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", s.Ports().DiscoveryPort))
	if err != nil {
		t.Fatal(err)
	}

	snapshots := cache.NewSnapshotCache(true, mock{}, nil)
	snapshots.SetSnapshot(id, makeSnapshot(s, t))
	server := xds.NewServer(snapshots, nil)
	discovery.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.GracefulStop()

	s.SetMixerSourceUID("pod.ns")

	if err := s.SetUp(); err != nil {
		t.Fatalf("Failed to setup test: %v", err)
	}
	defer s.TearDown()

	s.WaitEnvoyReady()

	// verify that bootstrap source.uid is present
	if _, _, err := env.HTTPGet(fmt.Sprintf("http://localhost:%d/echo", s.Ports().ClientProxyPort)); err != nil {
		t.Errorf("Failed in request: %v", err)
	}

	sourceUID := "kubernetes://pod.ns"
	s.VerifyCheck("http-outbound", fmt.Sprintf(checkAttributesOkOutbound, sourceUID))
	s.VerifyReport("http", fmt.Sprintf(reportAttributesOkOutbound, sourceUID))

	// Issues a GET echo request with 0 size body, forward some random source.uid
	attrs := mixerpb.Attributes{
		Attributes: map[string]*mixerpb.Attributes_AttributeValue{
			"source.uid": &mixerpb.Attributes_AttributeValue{Value: &mixerpb.Attributes_AttributeValue_StringValue{
				StringValue: "in-mesh-app",
			}},
		},
	}
	out, _ := attrs.Marshal()
	headers := map[string]string{
		"x-istio-attributes": base64.StdEncoding.EncodeToString(out),
	}
	if _, _, err := env.HTTPGetWithHeaders(fmt.Sprintf("http://localhost:%d/echo", s.Ports().ClientProxyPort), headers); err != nil {
		t.Errorf("Failed in request: %v", err)
	}
	sourceUID = "in-mesh-app"
	s.VerifyCheck("http-outbound", fmt.Sprintf(checkAttributesOkOutbound, sourceUID))
	s.VerifyReport("http", fmt.Sprintf(reportAttributesOkOutbound, sourceUID))
}

type mock struct{}

func (mock) ID(*core.Node) string {
	return id
}
func (mock) GetProxyServiceInstances(_ *model.Proxy) ([]*model.ServiceInstance, error) {
	return nil, nil
}
func (mock) GetService(_ model.Hostname) (*model.Service, error) { return nil, nil }
func (mock) InstancesByPort(_ model.Hostname, _ int, _ model.LabelsCollection) ([]*model.ServiceInstance, error) {
	return nil, nil
}
func (mock) ManagementPorts(_ string) model.PortList                               { return nil }
func (mock) Services() ([]*model.Service, error)                                   { return nil, nil }
func (mock) WorkloadHealthCheckInfo(_ string) model.ProbeList                      { return nil }
func (mock) GetIstioServiceAccounts(hostname model.Hostname, ports []int) []string { return nil }

const (
	id = "id"
)

var (
	svc = model.Service{
		Hostname: "svc.ns3",
		Attributes: model.ServiceAttributes{
			Name:      "svc",
			Namespace: "ns3",
			UID:       "istio://ns3/services/svc",
		},
	}
	mesh = &model.Environment{
		Mesh: &meshconfig.MeshConfig{
			MixerCheckServer:  "mixer_server:9091",
			MixerReportServer: "mixer_server:9091",
		},
		ServiceDiscovery: mock{},
	}
	pushContext = model.PushContext{
		ServiceByHostname: map[model.Hostname]*model.Service{
			model.Hostname("svc.ns3"): &svc,
		},
	}
	clientParams = plugin.InputParams{
		ListenerProtocol: plugin.ListenerProtocolHTTP,
		Env:              mesh,
		Node: &model.Proxy{
			ID:   "pod.ns",
			Type: model.Router,
		},
		Service: &svc,
		Push:    &pushContext,
	}
)

func makeRoute(cluster string) *v2.RouteConfiguration {
	return &v2.RouteConfiguration{
		Name: cluster,
		VirtualHosts: []route.VirtualHost{{
			Name:    cluster,
			Domains: []string{"*"},
			Routes: []route.Route{{
				Match: route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"}},
				Action: &route.Route_Route{Route: &route.RouteAction{
					ClusterSpecifier: &route.RouteAction_Cluster{Cluster: cluster},
				}},
			}},
		}},
	}
}

func makeListener(port uint16, route string) (*v2.Listener, *hcm.HttpConnectionManager) {
	return &v2.Listener{
			Name: route,
			Address: core.Address{Address: &core.Address_SocketAddress{SocketAddress: &core.SocketAddress{
				Address:       "127.0.0.1",
				PortSpecifier: &core.SocketAddress_PortValue{PortValue: uint32(port)}}}},
		}, &hcm.HttpConnectionManager{
			CodecType:  hcm.AUTO,
			StatPrefix: route,
			RouteSpecifier: &hcm.HttpConnectionManager_Rds{
				Rds: &hcm.Rds{RouteConfigName: route, ConfigSource: core.ConfigSource{
					ConfigSourceSpecifier: &core.ConfigSource_Ads{Ads: &core.AggregatedConfigSource{}},
				}},
			},
			HttpFilters: []*hcm.HttpFilter{{Name: util.Router}},
		}
}

func makeSnapshot(s *env.TestSetup, t *testing.T) cache.Snapshot {
	clientListener, clientManager := makeListener(s.Ports().ClientProxyPort, "outbound|||svc.ns3")
	clientRoute := makeRoute("outbound|||svc.ns3")

	p := mixer.NewPlugin()

	clientMutable := plugin.MutableObjects{Listener: clientListener, FilterChains: []plugin.FilterChain{{}}}
	if err := p.OnOutboundListener(&clientParams, &clientMutable); err != nil {
		t.Error(err)
	}
	clientManager.HttpFilters = append(clientMutable.FilterChains[0].HTTP, clientManager.HttpFilters...)
	clientListener.FilterChains = []listener.FilterChain{{Filters: []listener.Filter{{
		Name:       util.HTTPConnectionManager,
		ConfigType: &listener.Filter_TypedConfig{TypedConfig: pilotutil.MessageToAny(clientManager)},
	}}}}

	p.OnOutboundRouteConfiguration(&clientParams, clientRoute)

	return cache.Snapshot{
		Routes:    cache.NewResources("http", []cache.Resource{clientRoute}),
		Listeners: cache.NewResources("http", []cache.Resource{clientListener}),
	}
}
