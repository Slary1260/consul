package proxycfg

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mitchellh/copystructure"

	"github.com/hashicorp/consul/acl"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/consul/proto/pbpeering"
)

// TODO(ingress): Can we think of a better for this bag of data?
// A shared data structure that contains information about discovered upstreams
type ConfigSnapshotUpstreams struct {
	Leaf *structs.IssuedCert

	MeshConfig    *structs.MeshConfigEntry
	MeshConfigSet bool

	// DiscoveryChain is a map of UpstreamID -> CompiledDiscoveryChain's, and
	// is used to determine what services could be targeted by this upstream.
	// We then instantiate watches for those targets.
	DiscoveryChain map[UpstreamID]*structs.CompiledDiscoveryChain

	// WatchedDiscoveryChains is a map of UpstreamID -> CancelFunc's
	// in order to cancel any watches when the proxy's configuration is
	// changed. Ingress gateways and transparent proxies need this because
	// discovery chain watches are added and removed through the lifecycle
	// of a single proxycfg state instance.
	WatchedDiscoveryChains map[UpstreamID]context.CancelFunc

	// WatchedUpstreams is a map of UpstreamID -> (map of TargetID ->
	// CancelFunc's) in order to cancel any watches when the configuration is
	// changed.
	WatchedUpstreams map[UpstreamID]map[string]context.CancelFunc

	// WatchedUpstreamEndpoints is a map of UpstreamID -> (map of
	// TargetID -> CheckServiceNodes) and is used to determine the backing
	// endpoints of an upstream.
	WatchedUpstreamEndpoints map[UpstreamID]map[string]structs.CheckServiceNodes

	// WatchedPeerTrustBundles is a map of (PeerName -> CancelFunc) in order to cancel
	// watches for peer trust bundles any time the list of upstream peers changes.
	WatchedPeerTrustBundles map[string]context.CancelFunc

	// PeerTrustBundles is a map of (PeerName -> PeeringTrustBundle).
	// It is used to store trust bundles for upstream TLS transport sockets.
	PeerTrustBundles map[string]*pbpeering.PeeringTrustBundle

	// WatchedGateways is a map of UpstreamID -> (map of GatewayKey.String() ->
	// CancelFunc) in order to cancel watches for mesh gateways
	WatchedGateways map[UpstreamID]map[string]context.CancelFunc

	// WatchedGatewayEndpoints is a map of UpstreamID -> (map of
	// GatewayKey.String() -> CheckServiceNodes) and is used to determine the
	// backing endpoints of a mesh gateway.
	WatchedGatewayEndpoints map[UpstreamID]map[string]structs.CheckServiceNodes

	// UpstreamConfig is a map to an upstream's configuration.
	UpstreamConfig map[UpstreamID]*structs.Upstream

	// PassthroughEndpoints is a map of: UpstreamID -> (map of TargetID ->
	// (set of IP addresses)). It contains the upstream endpoints that
	// can be dialed directly by a transparent proxy.
	PassthroughUpstreams map[UpstreamID]map[string]map[string]struct{}

	// PassthroughIndices is a map of: address -> indexedTarget.
	// It is used to track the modify index associated with a passthrough address.
	// Tracking this index helps break ties when a single address is shared by
	// more than one upstream due to a race.
	PassthroughIndices map[string]indexedTarget

	// IntentionUpstreams is a set of upstreams inferred from intentions.
	//
	// This list only applies to proxies registered in 'transparent' mode.
	IntentionUpstreams map[UpstreamID]struct{}

	// PeerUpstreamEndpoints is a map of UpstreamID -> (set of IP addresses)
	// and used to determine the backing endpoints of an upstream in another
	// peer.
	PeerUpstreamEndpoints             map[UpstreamID]structs.CheckServiceNodes
	PeerUpstreamEndpointsUseHostnames map[UpstreamID]struct{}
}

// indexedTarget is used to associate the Raft modify index of a resource
// with the corresponding upstream target.
type indexedTarget struct {
	upstreamID UpstreamID
	targetID   string
	idx        uint64
}

type GatewayKey struct {
	Datacenter string
	Partition  string
}

func (k GatewayKey) String() string {
	resp := k.Datacenter
	if !acl.IsDefaultPartition(k.Partition) {
		resp = k.Partition + "." + resp
	}
	return resp
}

func (k GatewayKey) IsEmpty() bool {
	return k.Partition == "" && k.Datacenter == ""
}

func (k GatewayKey) Matches(dc, partition string) bool {
	return acl.EqualPartitions(k.Partition, partition) && k.Datacenter == dc
}

func gatewayKeyFromString(s string) GatewayKey {
	split := strings.SplitN(s, ".", 2)

	if len(split) == 1 {
		return GatewayKey{Datacenter: split[0], Partition: acl.DefaultPartitionName}
	}
	return GatewayKey{Partition: split[0], Datacenter: split[1]}
}

type configSnapshotConnectProxy struct {
	ConfigSnapshotUpstreams

	PeeringTrustBundlesSet bool
	PeeringTrustBundles    []*pbpeering.PeeringTrustBundle

	WatchedServiceChecks   map[structs.ServiceID][]structs.CheckType // TODO: missing garbage collection
	PreparedQueryEndpoints map[UpstreamID]structs.CheckServiceNodes  // DEPRECATED:see:WatchedUpstreamEndpoints

	// NOTE: Intentions stores a list of lists as returned by the Intentions
	// Match RPC. So far we only use the first list as the list of matching
	// intentions.
	Intentions    structs.Intentions
	IntentionsSet bool
}

// isEmpty is a test helper
func (c *configSnapshotConnectProxy) isEmpty() bool {
	if c == nil {
		return true
	}
	return c.Leaf == nil &&
		!c.IntentionsSet &&
		len(c.DiscoveryChain) == 0 &&
		len(c.WatchedDiscoveryChains) == 0 &&
		len(c.WatchedUpstreams) == 0 &&
		len(c.WatchedUpstreamEndpoints) == 0 &&
		len(c.WatchedPeerTrustBundles) == 0 &&
		len(c.PeerTrustBundles) == 0 &&
		len(c.WatchedGateways) == 0 &&
		len(c.WatchedGatewayEndpoints) == 0 &&
		len(c.WatchedServiceChecks) == 0 &&
		len(c.PreparedQueryEndpoints) == 0 &&
		len(c.UpstreamConfig) == 0 &&
		len(c.PassthroughUpstreams) == 0 &&
		len(c.IntentionUpstreams) == 0 &&
		!c.PeeringTrustBundlesSet &&
		!c.MeshConfigSet &&
		len(c.PeerUpstreamEndpoints) == 0 &&
		len(c.PeerUpstreamEndpointsUseHostnames) == 0
}

type configSnapshotTerminatingGateway struct {
	MeshConfig    *structs.MeshConfigEntry
	MeshConfigSet bool

	// WatchedServices is a map of service name to a cancel function. This cancel
	// function is tied to the watch of linked service instances for the given
	// id. If the linked services watch would indicate the removal of
	// a service altogether we then cancel watching that service for its endpoints.
	WatchedServices map[structs.ServiceName]context.CancelFunc

	// WatchedIntentions is a map of service name to a cancel function.
	// This cancel function is tied to the watch of intentions for linked services.
	// As with WatchedServices, intention watches will be cancelled when services
	// are no longer linked to the gateway.
	WatchedIntentions map[structs.ServiceName]context.CancelFunc

	// NOTE: Intentions stores a map of list of lists as returned by the Intentions
	// Match RPC. So far we only use the first list as the list of matching
	// intentions.
	//
	// A key being present implies that we have gotten at least one watch reply for the
	// service. This is logically the same as ConnectProxy.IntentionsSet==true
	Intentions map[structs.ServiceName]structs.Intentions

	// WatchedLeaves is a map of ServiceName to a cancel function.
	// This cancel function is tied to the watch of leaf certs for linked services.
	// As with WatchedServices, leaf watches will be cancelled when services
	// are no longer linked to the gateway.
	WatchedLeaves map[structs.ServiceName]context.CancelFunc

	// ServiceLeaves is a map of ServiceName to a leaf cert.
	// Terminating gateways will present different certificates depending
	// on the service that the caller is trying to reach.
	ServiceLeaves map[structs.ServiceName]*structs.IssuedCert

	// WatchedConfigs is a map of ServiceName to a cancel function. This cancel
	// function is tied to the watch of service configs for linked services. As
	// with WatchedServices, service config watches will be cancelled when
	// services are no longer linked to the gateway.
	WatchedConfigs map[structs.ServiceName]context.CancelFunc

	// ServiceConfigs is a map of service name to the resolved service config
	// for that service.
	ServiceConfigs map[structs.ServiceName]*structs.ServiceConfigResponse

	// WatchedResolvers is a map of ServiceName to a cancel function.
	// This cancel function is tied to the watch of resolvers for linked services.
	// As with WatchedServices, resolver watches will be cancelled when services
	// are no longer linked to the gateway.
	WatchedResolvers map[structs.ServiceName]context.CancelFunc

	// ServiceResolvers is a map of service name to an associated
	// service-resolver config entry for that service.
	ServiceResolvers    map[structs.ServiceName]*structs.ServiceResolverConfigEntry
	ServiceResolversSet map[structs.ServiceName]bool

	// ServiceGroups is a map of service name to the service instances of that
	// service in the local datacenter.
	ServiceGroups map[structs.ServiceName]structs.CheckServiceNodes

	// GatewayServices is a map of service name to the config entry association
	// between the gateway and a service. TLS configuration stored here is
	// used for TLS origination from the gateway to the linked service.
	GatewayServices map[structs.ServiceName]structs.GatewayService

	// HostnameServices is a map of service name to service instances with a hostname as the address.
	// If hostnames are configured they must be provided to Envoy via CDS not EDS.
	HostnameServices map[structs.ServiceName]structs.CheckServiceNodes
}

// ValidServices returns the list of service keys that have enough data to be emitted.
func (c *configSnapshotTerminatingGateway) ValidServices() []structs.ServiceName {
	out := make([]structs.ServiceName, 0, len(c.ServiceGroups))
	for svc := range c.ServiceGroups {
		// It only counts if ALL of our watches have come back (with data or not).

		// Skip the service if we don't know if there is a resolver or not.
		if _, ok := c.ServiceResolversSet[svc]; !ok {
			continue
		}

		// Skip the service if we don't have a cert to present for mTLS.
		if cert, ok := c.ServiceLeaves[svc]; !ok || cert == nil {
			continue
		}

		// Skip the service if we haven't gotten our intentions yet.
		if _, intentionsSet := c.Intentions[svc]; !intentionsSet {
			continue
		}

		// Skip the service if we haven't gotten our service config yet to know
		// the protocol.
		if _, ok := c.ServiceConfigs[svc]; !ok {
			continue
		}

		out = append(out, svc)
	}
	return out
}

// isEmpty is a test helper
func (c *configSnapshotTerminatingGateway) isEmpty() bool {
	if c == nil {
		return true
	}
	return len(c.ServiceLeaves) == 0 &&
		len(c.WatchedLeaves) == 0 &&
		len(c.WatchedIntentions) == 0 &&
		len(c.Intentions) == 0 &&
		len(c.ServiceGroups) == 0 &&
		len(c.WatchedServices) == 0 &&
		len(c.ServiceResolvers) == 0 &&
		len(c.ServiceResolversSet) == 0 &&
		len(c.WatchedResolvers) == 0 &&
		len(c.ServiceConfigs) == 0 &&
		len(c.WatchedConfigs) == 0 &&
		len(c.GatewayServices) == 0 &&
		len(c.HostnameServices) == 0 &&
		!c.MeshConfigSet
}

type configSnapshotMeshGateway struct {
	// WatchedServices is a map of service name to a cancel function. This cancel
	// function is tied to the watch of connect enabled services for the given
	// id. If the main datacenter services watch would indicate the removal of
	// a service altogether we then cancel watching that service for its
	// connect endpoints.
	WatchedServices map[structs.ServiceName]context.CancelFunc

	// WatchedServicesSet indicates that the watch on the datacenters services
	// has completed. Even when there are no connect services, this being set
	// (and the Connect roots being available) will be enough for the config
	// snapshot to be considered valid. In the case of Envoy, this allows it to
	// start its listeners even when no services would be proxied and allow its
	// health check to pass.
	WatchedServicesSet bool

	// WatchedGateways is a map of GatewayKeys to a cancel function.
	// This cancel function is tied to the watch of mesh-gateway services in
	// that datacenter/partition.
	WatchedGateways map[string]context.CancelFunc

	// ServiceGroups is a map of service name to the service instances of that
	// service in the local datacenter.
	ServiceGroups map[structs.ServiceName]structs.CheckServiceNodes

	// ServiceResolvers is a map of service name to an associated
	// service-resolver config entry for that service.
	ServiceResolvers map[structs.ServiceName]*structs.ServiceResolverConfigEntry

	// GatewayGroups is a map of datacenter names to services of kind
	// mesh-gateway in that datacenter.
	GatewayGroups map[string]structs.CheckServiceNodes

	// FedStateGateways is a map of datacenter names to mesh gateways in that
	// datacenter.
	FedStateGateways map[string]structs.CheckServiceNodes

	// ConsulServers is the list of consul servers in this datacenter.
	ConsulServers structs.CheckServiceNodes

	// HostnameDatacenters is a map of datacenters to mesh gateway instances with a hostname as the address.
	// If hostnames are configured they must be provided to Envoy via CDS not EDS.
	HostnameDatacenters map[string]structs.CheckServiceNodes

	// TODO(peering):
	ExportedServicesSlice []structs.ServiceName

	// TODO(peering): svc -> peername slice
	ExportedServicesWithPeers map[structs.ServiceName][]string

	// TODO(peering):  discard this maybe
	WatchedExportedServices map[string]structs.ServiceList

	// TODO(peering):
	WatchedExportedServicesSet bool

	// TODO(peering):
	DiscoveryChain map[structs.ServiceName]*structs.CompiledDiscoveryChain

	// TODO(peering):
	WatchedDiscoveryChains map[structs.ServiceName]context.CancelFunc
}

func (c *configSnapshotMeshGateway) IsServiceExported(svc structs.ServiceName) bool {
	if c == nil || len(c.ExportedServicesWithPeers) == 0 {
		return false
	}

	_, ok := c.ExportedServicesWithPeers[svc]
	return ok
}

func (c *configSnapshotMeshGateway) GatewayKeys() []GatewayKey {
	sz1, sz2 := len(c.GatewayGroups), len(c.FedStateGateways)

	sz := sz1
	if sz2 > sz1 {
		sz = sz2
	}

	keys := make([]GatewayKey, 0, sz)
	for key := range c.FedStateGateways {
		keys = append(keys, gatewayKeyFromString(key))
	}
	for key := range c.GatewayGroups {
		gk := gatewayKeyFromString(key)
		if _, ok := c.FedStateGateways[gk.Datacenter]; !ok {
			keys = append(keys, gk)
		}
	}

	// Always sort the results to ensure we generate deterministic things over
	// xDS, such as mesh-gateway listener filter chains.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Datacenter != keys[j].Datacenter {
			return keys[i].Datacenter < keys[j].Datacenter
		}
		return keys[i].Partition < keys[j].Partition
	})
	return keys
}

// isEmpty is a test helper
func (c *configSnapshotMeshGateway) isEmpty() bool {
	if c == nil {
		return true
	}
	return len(c.WatchedServices) == 0 &&
		!c.WatchedServicesSet &&
		len(c.WatchedGateways) == 0 &&
		len(c.ServiceGroups) == 0 &&
		len(c.ServiceResolvers) == 0 &&
		len(c.GatewayGroups) == 0 &&
		len(c.FedStateGateways) == 0 &&
		len(c.ConsulServers) == 0 &&
		len(c.HostnameDatacenters) == 0 &&
		c.isEmptyPeering()
}

// isEmptyPeering is a test helper
func (c *configSnapshotMeshGateway) isEmptyPeering() bool {
	if c == nil {
		return true
	}

	return len(c.ExportedServicesSlice) == 0 &&
		len(c.ExportedServicesWithPeers) == 0 &&
		len(c.WatchedExportedServices) == 0 &&
		!c.WatchedExportedServicesSet &&
		len(c.DiscoveryChain) == 0 &&
		len(c.WatchedDiscoveryChains) == 0
}

type configSnapshotIngressGateway struct {
	ConfigSnapshotUpstreams

	// TLSConfig is the gateway-level TLS configuration. Listener/service level
	// config is preserved in the Listeners map below.
	TLSConfig structs.GatewayTLSConfig

	// GatewayConfigLoaded is used to determine if we have received the initial
	// ingress-gateway config entry yet.
	GatewayConfigLoaded bool

	// Hosts is the list of extra host entries to add to our leaf cert's DNS SANs.
	Hosts    []string
	HostsSet bool

	// LeafCertWatchCancel is a CancelFunc to use when refreshing this gateway's
	// leaf cert watch with different parameters.
	LeafCertWatchCancel context.CancelFunc

	// Upstreams is a list of upstreams this ingress gateway should serve traffic
	// to. This is constructed from the ingress-gateway config entry, and uses
	// the GatewayServices RPC to retrieve them.
	Upstreams map[IngressListenerKey]structs.Upstreams

	// UpstreamsSet is the unique set of UpstreamID the gateway routes to.
	UpstreamsSet map[UpstreamID]struct{}

	// Listeners is the original listener config from the ingress-gateway config
	// entry to save us trying to pass fields through Upstreams
	Listeners map[IngressListenerKey]structs.IngressListener
}

// isEmpty is a test helper
func (c *configSnapshotIngressGateway) isEmpty() bool {
	if c == nil {
		return true
	}
	return len(c.Upstreams) == 0 &&
		len(c.UpstreamsSet) == 0 &&
		len(c.DiscoveryChain) == 0 &&
		len(c.WatchedUpstreams) == 0 &&
		len(c.WatchedUpstreamEndpoints) == 0 &&
		!c.MeshConfigSet
}

type IngressListenerKey struct {
	Protocol string
	Port     int
}

func (k *IngressListenerKey) RouteName() string {
	return fmt.Sprintf("%d", k.Port)
}

func IngressListenerKeyFromGWService(s structs.GatewayService) IngressListenerKey {
	return IngressListenerKey{Protocol: s.Protocol, Port: s.Port}
}

func IngressListenerKeyFromListener(l structs.IngressListener) IngressListenerKey {
	return IngressListenerKey{Protocol: l.Protocol, Port: l.Port}
}

// ConfigSnapshot captures all the resulting config needed for a proxy instance.
// It is meant to be point-in-time coherent and is used to deliver the current
// config state to observers who need it to be pushed in (e.g. XDS server).
type ConfigSnapshot struct {
	Kind                  structs.ServiceKind
	Service               string
	ProxyID               ProxyID
	Address               string
	Port                  int
	ServiceMeta           map[string]string
	TaggedAddresses       map[string]structs.ServiceAddress
	Proxy                 structs.ConnectProxyConfig
	Datacenter            string
	IntentionDefaultAllow bool
	Locality              GatewayKey

	ServerSNIFn ServerSNIFunc
	Roots       *structs.IndexedCARoots

	// connect-proxy specific
	ConnectProxy configSnapshotConnectProxy

	// terminating-gateway specific
	TerminatingGateway configSnapshotTerminatingGateway

	// mesh-gateway specific
	MeshGateway configSnapshotMeshGateway

	// ingress-gateway specific
	IngressGateway configSnapshotIngressGateway
}

// Valid returns whether or not the snapshot has all required fields filled yet.
func (s *ConfigSnapshot) Valid() bool {
	switch s.Kind {
	case structs.ServiceKindConnectProxy:
		if s.Proxy.Mode == structs.ProxyModeTransparent && !s.ConnectProxy.MeshConfigSet {
			return false
		}
		return s.Roots != nil &&
			s.ConnectProxy.Leaf != nil &&
			s.ConnectProxy.IntentionsSet &&
			s.ConnectProxy.MeshConfigSet

	case structs.ServiceKindTerminatingGateway:
		return s.Roots != nil &&
			s.TerminatingGateway.MeshConfigSet

	case structs.ServiceKindMeshGateway:
		if s.ServiceMeta[structs.MetaWANFederationKey] == "1" {
			if len(s.MeshGateway.ConsulServers) == 0 {
				return false
			}
		}
		return s.Roots != nil &&
			(s.MeshGateway.WatchedServicesSet || len(s.MeshGateway.ServiceGroups) > 0) &&
			s.MeshGateway.WatchedExportedServicesSet

	case structs.ServiceKindIngressGateway:
		return s.Roots != nil &&
			s.IngressGateway.Leaf != nil &&
			s.IngressGateway.GatewayConfigLoaded &&
			s.IngressGateway.HostsSet &&
			s.IngressGateway.MeshConfigSet
	default:
		return false
	}
}

// Clone makes a deep copy of the snapshot we can send to other goroutines
// without worrying that they will racily read or mutate shared maps etc.
func (s *ConfigSnapshot) Clone() (*ConfigSnapshot, error) {
	snapCopy, err := copystructure.Copy(s)
	if err != nil {
		return nil, err
	}

	snap := snapCopy.(*ConfigSnapshot)

	// nil these out as anything receiving one of these clones does not need them and should never "cancel" our watches
	switch s.Kind {
	case structs.ServiceKindConnectProxy:
		// common with connect-proxy and ingress-gateway
		snap.ConnectProxy.WatchedUpstreams = nil
		snap.ConnectProxy.WatchedGateways = nil
		snap.ConnectProxy.WatchedDiscoveryChains = nil
		snap.ConnectProxy.WatchedPeerTrustBundles = nil
	case structs.ServiceKindTerminatingGateway:
		snap.TerminatingGateway.WatchedServices = nil
		snap.TerminatingGateway.WatchedIntentions = nil
		snap.TerminatingGateway.WatchedLeaves = nil
		snap.TerminatingGateway.WatchedConfigs = nil
		snap.TerminatingGateway.WatchedResolvers = nil
	case structs.ServiceKindMeshGateway:
		snap.MeshGateway.WatchedGateways = nil
		snap.MeshGateway.WatchedServices = nil
	case structs.ServiceKindIngressGateway:
		// common with connect-proxy and ingress-gateway
		snap.IngressGateway.WatchedUpstreams = nil
		snap.IngressGateway.WatchedGateways = nil
		snap.IngressGateway.WatchedDiscoveryChains = nil
		snap.IngressGateway.WatchedPeerTrustBundles = nil
		// only ingress-gateway
		snap.IngressGateway.LeafCertWatchCancel = nil
	}

	return snap, nil
}

func (s *ConfigSnapshot) Leaf() *structs.IssuedCert {
	switch s.Kind {
	case structs.ServiceKindConnectProxy:
		return s.ConnectProxy.Leaf
	case structs.ServiceKindIngressGateway:
		return s.IngressGateway.Leaf
	default:
		return nil
	}
}

// RootPEMs returns all PEM-encoded public certificates for the root CA.
func (s *ConfigSnapshot) RootPEMs() string {
	var rootPEMs string
	for _, root := range s.Roots.Roots {
		rootPEMs += lib.EnsureTrailingNewline(root.RootCert)
	}
	return rootPEMs
}

func (s *ConfigSnapshot) MeshConfig() *structs.MeshConfigEntry {
	switch s.Kind {
	case structs.ServiceKindConnectProxy:
		return s.ConnectProxy.MeshConfig
	case structs.ServiceKindIngressGateway:
		return s.IngressGateway.MeshConfig
	case structs.ServiceKindTerminatingGateway:
		return s.TerminatingGateway.MeshConfig
	default:
		return nil
	}
}

func (s *ConfigSnapshot) MeshConfigTLSIncoming() *structs.MeshDirectionalTLSConfig {
	mesh := s.MeshConfig()
	if mesh == nil || mesh.TLS == nil {
		return nil
	}
	return mesh.TLS.Incoming
}

func (s *ConfigSnapshot) MeshConfigTLSOutgoing() *structs.MeshDirectionalTLSConfig {
	mesh := s.MeshConfig()
	if mesh == nil || mesh.TLS == nil {
		return nil
	}
	return mesh.TLS.Outgoing
}

func (u *ConfigSnapshotUpstreams) UpstreamPeerMeta(uid UpstreamID) structs.PeeringServiceMeta {
	nodes := u.PeerUpstreamEndpoints[uid]
	if len(nodes) == 0 {
		return structs.PeeringServiceMeta{}
	}

	// In agent/rpc/peering/subscription_manager.go we denormalize the
	// PeeringServiceMeta data onto each replicated service instance to convey
	// this information back to the importing side of the peering.
	//
	// This data is guaranteed (subject to any eventual consistency lag around
	// updates) to be the same across all instances, so we only need to take
	// the first item.
	//
	// TODO(peering): consider replicating this "common to all instances" data
	// using a different replication type and persist it separately in the
	// catalog to avoid this weird construction.
	csn := nodes[0]
	if csn.Service == nil {
		return structs.PeeringServiceMeta{}
	}
	return *csn.Service.Connect.PeerMeta
}

func (u *ConfigSnapshotUpstreams) PeeredUpstreamIDs() []UpstreamID {
	out := make([]UpstreamID, 0, len(u.UpstreamConfig))
	for uid := range u.UpstreamConfig {
		if uid.Peer == "" {
			continue
		}

		if _, ok := u.PeerTrustBundles[uid.Peer]; uid.Peer != "" && !ok {
			// The trust bundle for this upstream is not available yet, skip for now.
			continue
		}

		out = append(out, uid)
	}
	return out
}
