package tunneling

import (
	envoy_config_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_config_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	envoy_config_listener_v3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	envoy_config_route_v3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	envoytcp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/golang/protobuf/ptypes/duration"
	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/solo-io/gloo/projects/gloo/pkg/plugins"
	"github.com/solo-io/gloo/projects/gloo/pkg/translator"
	"github.com/solo-io/gloo/projects/gloo/pkg/utils"
	"k8s.io/apimachinery/pkg/util/sets"
)

var (
	_ plugins.Plugin                  = new(plugin)
	_ plugins.ResourceGeneratorPlugin = new(plugin)
)

const (
	ExtensionName = "tunneling"
)

type plugin struct{}

func NewPlugin() *plugin {
	return &plugin{}
}

func (p *plugin) Name() string {
	return ExtensionName
}

func (p *plugin) Init(_ plugins.InitParams) {
}

func (p *plugin) GeneratedResources(params plugins.Params,
	inClusters []*envoy_config_cluster_v3.Cluster,
	inEndpoints []*envoy_config_endpoint_v3.ClusterLoadAssignment,
	inRouteConfigurations []*envoy_config_route_v3.RouteConfiguration,
	inListeners []*envoy_config_listener_v3.Listener,
) ([]*envoy_config_cluster_v3.Cluster, []*envoy_config_endpoint_v3.ClusterLoadAssignment, []*envoy_config_route_v3.RouteConfiguration, []*envoy_config_listener_v3.Listener, error) {

	var generatedClusters []*envoy_config_cluster_v3.Cluster
	var generatedListeners []*envoy_config_listener_v3.Listener

	upstreams := params.Snapshot.Upstreams

	// keep track of clusters we've seen in case of multiple routes to same cluster
	processedClusters := sets.NewString()

	// find all the route config that points to upstreams with tunneling
	for _, rtConfig := range inRouteConfigurations {
		for _, vh := range rtConfig.GetVirtualHosts() {
			for _, rt := range vh.GetRoutes() {
				rtAction := rt.GetRoute()
				// we do not handle the weighted cluster or cluster header cases
				if cluster := rtAction.GetCluster(); cluster != "" {

					ref, err := translator.ClusterToUpstreamRef(cluster)
					if err != nil {
						// return what we have so far, so that any modified input resources can still route
						// successfully to their generated targets
						return generatedClusters, nil, nil, generatedListeners, nil
					}

					us, err := upstreams.Find(ref.GetNamespace(), ref.GetName())
					if err != nil {
						// return what we have so far, so that any modified input resources can still route
						// successfully to their generated targets
						return generatedClusters, nil, nil, generatedListeners, nil
					}

					// the existence of this value is our indicator that this is a tunneling upstream
					tunnelingHostname := us.GetHttpProxyHostname().GetValue()
					if tunnelingHostname == "" {
						continue
					}

					var tunnelingHeaders []*envoy_config_core_v3.HeaderValueOption
					for _, header := range us.GetHttpConnectHeaders() {
						tunnelingHeaders = append(tunnelingHeaders, &envoy_config_core_v3.HeaderValueOption{
							Header: &envoy_config_core_v3.HeaderValue{
								Key:   header.GetKey(),
								Value: header.GetValue(),
							},
							Append: &wrappers.BoolValue{Value: false},
						})
					}

					selfCluster := "solo_io_generated_self_cluster_" + cluster
					selfPipe := "@/" + cluster // use an in-memory pipe to ourselves (only works on linux)

					// update the old cluster to route to ourselves first
					rtAction.ClusterSpecifier = &envoy_config_route_v3.RouteAction_Cluster{Cluster: selfCluster}

					// we only want to generate a new encapsulating cluster and pipe to ourselves if we have not done so already
					if processedClusters.Has(cluster) {
						continue
					}
					var originalTransportSocket *envoy_config_core_v3.TransportSocket
					for _, inCluster := range inClusters {
						if inCluster.GetName() == cluster {
							if inCluster.GetTransportSocket() != nil {
								tmp := *inCluster.GetTransportSocket()
								originalTransportSocket = &tmp
							}
							// we copy the transport socket to the generated cluster.
							// the generated cluster will use upstream TLS context to leverage TLS origination;
							// when we encapsulate in HTTP Connect the tcp data being proxied will
							// be encrypted (thus we don't need the original transport socket metadata here)
							inCluster.TransportSocket = nil
							inCluster.TransportSocketMatches = nil

							if us.GetHttpConnectSslConfig() == nil {
								break
							}
							// user told us to configure ssl for the http connect proxy
							cfg, err := utils.NewSslConfigTranslator().ResolveUpstreamSslConfig(params.Snapshot.Secrets, us.GetHttpConnectSslConfig())
							if err != nil {
								// return what we have so far, so that any modified input resources can still route
								// successfully to their generated targets
								return generatedClusters, nil, nil, generatedListeners, nil
							}
							typedConfig, err := utils.MessageToAny(cfg)
							if err != nil {
								return nil, nil, nil, nil, err
							}
							inCluster.TransportSocket = &envoy_config_core_v3.TransportSocket{
								Name:       wellknown.TransportSocketTls,
								ConfigType: &envoy_config_core_v3.TransportSocket_TypedConfig{TypedConfig: typedConfig},
							}
							break
						}
					}
					generatedClusters = append(generatedClusters, generateSelfCluster(selfCluster, selfPipe, originalTransportSocket))
					forwardingTcpListener, err := generateForwardingTcpListener(cluster, selfPipe, tunnelingHostname, tunnelingHeaders)
					if err != nil {
						return nil, nil, nil, nil, err
					}
					generatedListeners = append(generatedListeners, forwardingTcpListener)
					processedClusters.Insert(cluster)
				}
			}
		}
	}

	return generatedClusters, nil, nil, generatedListeners, nil
}

// the initial route is updated to route to this generated cluster, which routes envoy back to itself (to the
// generated TCP listener, which forwards to the original destination)
//
// the purpose of doing this is to allow both the HTTP Connection Manager filter and TCP filter to run.
// the HTTP Connection Manager runs to allow route-level matching on HTTP parameters (such as request path),
// but then we forward the bytes as raw TCP to the HTTP Connect proxy (which can only be done on a TCP listener)
func generateSelfCluster(selfCluster, selfPipe string, originalTransportSocket *envoy_config_core_v3.TransportSocket) *envoy_config_cluster_v3.Cluster {
	return &envoy_config_cluster_v3.Cluster{
		ClusterDiscoveryType: &envoy_config_cluster_v3.Cluster_Type{
			Type: envoy_config_cluster_v3.Cluster_STATIC,
		},
		ConnectTimeout:  &duration.Duration{Seconds: 5},
		Name:            selfCluster,
		TransportSocket: originalTransportSocket,
		LoadAssignment: &envoy_config_endpoint_v3.ClusterLoadAssignment{
			ClusterName: selfCluster,
			Endpoints: []*envoy_config_endpoint_v3.LocalityLbEndpoints{
				{
					LbEndpoints: []*envoy_config_endpoint_v3.LbEndpoint{
						{
							HostIdentifier: &envoy_config_endpoint_v3.LbEndpoint_Endpoint{
								Endpoint: &envoy_config_endpoint_v3.Endpoint{
									Address: &envoy_config_core_v3.Address{
										Address: &envoy_config_core_v3.Address_Pipe{
											Pipe: &envoy_config_core_v3.Pipe{
												Path: selfPipe,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// the generated cluster routes to this generated listener, which forwards TCP traffic to an HTTP Connect proxy
func generateForwardingTcpListener(cluster, selfPipe, tunnelingHostname string, tunnelingHeadersToAdd []*envoy_config_core_v3.HeaderValueOption) (*envoy_config_listener_v3.Listener, error) {
	cfg := &envoytcp.TcpProxy{
		StatPrefix:       "soloioTcpStats" + cluster,
		TunnelingConfig:  &envoytcp.TcpProxy_TunnelingConfig{Hostname: tunnelingHostname, HeadersToAdd: tunnelingHeadersToAdd},
		ClusterSpecifier: &envoytcp.TcpProxy_Cluster{Cluster: cluster}, // route to original target
	}
	typedConfig, err := utils.MessageToAny(cfg)
	if err != nil {
		return nil, err
	}
	return &envoy_config_listener_v3.Listener{
		Name: "solo_io_generated_self_listener_" + cluster,
		Address: &envoy_config_core_v3.Address{
			Address: &envoy_config_core_v3.Address_Pipe{
				Pipe: &envoy_config_core_v3.Pipe{
					Path: selfPipe,
				},
			},
		},
		FilterChains: []*envoy_config_listener_v3.FilterChain{
			{
				Filters: []*envoy_config_listener_v3.Filter{
					{
						Name: "tcp",
						ConfigType: &envoy_config_listener_v3.Filter_TypedConfig{
							TypedConfig: typedConfig,
						},
					},
				},
			},
		},
	}, nil
}
