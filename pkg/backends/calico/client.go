// Copyright (c) 2017-2020 Tigera, Inc. All rights reserved.
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
package calico

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/kelseyhightower/confd/pkg/buildinfo"
	"github.com/kelseyhightower/confd/pkg/config"
	logutils "github.com/kelseyhightower/confd/pkg/log"
	log "github.com/sirupsen/logrus"

	"github.com/kelseyhightower/confd/pkg/resource/template"

	"github.com/projectcalico/libcalico-go/lib/apiconfig"
	apiv3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	"github.com/projectcalico/libcalico-go/lib/backend/syncersv1/bgpsyncer"
	"github.com/projectcalico/libcalico-go/lib/backend/syncersv1/updateprocessors"
	"github.com/projectcalico/libcalico-go/lib/backend/watchersyncer"
	"github.com/projectcalico/libcalico-go/lib/clientv3"
	lerr "github.com/projectcalico/libcalico-go/lib/errors"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/projectcalico/libcalico-go/lib/numorstring"
	"github.com/projectcalico/libcalico-go/lib/options"
	"github.com/projectcalico/libcalico-go/lib/selector"
	"github.com/projectcalico/typha/pkg/syncclientutils"
	"github.com/projectcalico/typha/pkg/syncproto"
)

const globalLogging = "/calico/bgp/v1/global/loglevel"

const (
	DEBUGGING_CONFIGURATION_LOG_LEVEL = "CONFD_DEBUGGING_CONFIGURATION_LOG_LEVEL"
)

// Handle a few keys that we need to default if not specified.
var globalDefaults = map[string]string{
	"/calico/bgp/v1/global/as_num":    "64512",
	"/calico/bgp/v1/global/node_mesh": `{"enabled": true}`,
	globalLogging:                     "info",
}

// backendClientAccessor is an interface to access the backend client from the main v2 client.
type backendClientAccessor interface {
	Backend() api.Client
}

func NewClient(confdConfig *config.Config) (clientv3.Interface, error) {
	// Load the client clientCfg.  This loads from the environment if a filename
	// has not been specified.
	clientCfg, err := apiconfig.LoadClientConfig(confdConfig.CalicoConfig)
	if err != nil {
		log.Errorf("Failed to load Calico client configuration: %v", err)
		return nil, err
	}

	cc, err := clientv3.New(*clientCfg)
	if err != nil {
		log.Errorf("Failed to create main Calico client: %v", err)
		return nil, err
	}
	return cc, nil
}

// TriggerLogLevelSetting is just a wrapper around updateLogLevel
func TriggerLogLevelSetting(cc interface{}) {
	if c, ok := cc.(*client); ok {
		c.updateLogLevel()
	} else {
		log.Warn("failed to get client.")
	}
}

func NewCalicoClient(confdConfig *config.Config) (*client, error) {
	// Load the client clientCfg.  This loads from the environment if a filename
	// has not been specified.
	clientCfg, err := apiconfig.LoadClientConfig(confdConfig.CalicoConfig)
	if err != nil {
		log.Errorf("Failed to load Calico client configuration: %v", err)
		return nil, err
	}

	cc, err := NewClient(confdConfig)
	if err != nil {
		log.Errorf("Failed to create main Calico client: %v", err)
		return nil, err
	}

	// Query the current BGP configuration to determine if the node to node mesh is enabled or
	// not.  If it is we need to monitor all node configuration.  If it is not enabled then we
	// only need to monitor our own node.  If this setting changes, we terminate confd (so that
	// when restarted it will start watching the correct resources).
	cfg, err := cc.BGPConfigurations().Get(
		context.Background(),
		"default",
		options.GetOptions{},
	)
	if _, ok := err.(lerr.ErrorResourceDoesNotExist); err != nil && !ok {
		// Failed to get the BGP configuration (and not because it doesn't exist).
		// Exit.
		log.Errorf("Failed to query current BGP settings: %v", err)
		return nil, err
	}
	nodeMeshEnabled := true
	if cfg != nil && cfg.Spec.NodeToNodeMeshEnabled != nil {
		nodeMeshEnabled = *cfg.Spec.NodeToNodeMeshEnabled
	}

	// We know the v2 client implements the backendClientAccessor interface.  Use it to
	// get the backend client.
	bc := cc.(backendClientAccessor).Backend()

	// Create the client.  Initialize the cache revision to 1 so that the watcher
	// code can handle the first iteration by always rendering.
	c := &client{
		client:            bc,
		cache:             make(map[string]string),
		peeringCache:      make(map[string]string),
		cacheRevision:     1,
		revisionsByPrefix: make(map[string]uint64),
		nodeMeshEnabled:   nodeMeshEnabled,
		nodeLabels:        make(map[string]map[string]string),
		bgpPeers:          make(map[string]*apiv3.BGPPeer),
		sourceReady:       make(map[string]bool),

		// This channel, for the syncer calling OnUpdates and OnStatusUpdated, has 0
		// capacity so that the caller blocks in the same way as it did before when its
		// calls were processed synchronously.
		syncerC: make(chan interface{}),
	}
	for k, v := range globalDefaults {
		c.cache[k] = v
	}

	// Create a conditional that we use to wake up all of the watcher threads when there
	// may some actionable updates.
	c.watcherCond = sync.NewCond(&c.cacheLock)

	// Increment the waitForSync wait group.  This blocks the GetValues call until the
	// syncer has completed its initial snapshot and is in sync.
	c.waitForSync.Add(1)

	// Get cluster CIDRs. Prefer the env var, if specified.
	clusterCIDRs := []string{}
	if clusterCIDR := os.Getenv(envAdvertiseClusterIPs); len(clusterCIDR) != 0 {
		clusterCIDRs = []string{clusterCIDR}
	} else if cfg != nil && cfg.Spec.ServiceClusterIPs != nil {
		for _, c := range cfg.Spec.ServiceClusterIPs {
			clusterCIDRs = append(clusterCIDRs, c.CIDR)
		}
	}
	// Note: do this initial update before starting the syncer, so there's no chance of this
	// racing with syncer-derived updates.
	c.onClusterIPsUpdate(clusterCIDRs)

	// Get external IP CIDRs.
	externalCIDRs := []string{}
	if cfg != nil && cfg.Spec.ServiceExternalIPs != nil {
		for _, c := range cfg.Spec.ServiceExternalIPs {
			externalCIDRs = append(externalCIDRs, c.CIDR)
		}
	}
	// Note: do this initial update before starting the syncer, so there's no chance of this
	// racing with syncer-derived updates.
	c.onExternalIPsUpdate(externalCIDRs)

	// Start the main syncer loop.  If the node-to-node mesh is enabled then we need to
	// monitor all nodes.  If this setting changes (which we will monitor in the OnUpdates
	// callback) then we terminate confd - the calico/node init process will restart the
	// confd process.
	c.nodeLogKey = fmt.Sprintf("/calico/bgp/v1/host/%s/loglevel", template.NodeName)
	c.nodeV1Processor = updateprocessors.NewBGPNodeUpdateProcessor(clientCfg.Spec.K8sUsePodCIDR)

	if syncclientutils.MustStartSyncerClientIfTyphaConfigured(
		&confdConfig.Typha, syncproto.SyncerTypeBGP,
		buildinfo.GitVersion, template.NodeName, fmt.Sprintf("confd %s", buildinfo.GitVersion),
		c,
	) {
		log.Debug("Using typha syncclient")
	} else {
		// Use the syncer locally.
		log.Debug("Using local syncer")
		c.syncer = bgpsyncer.New(c.client, c, template.NodeName, clientCfg.Spec)
		c.syncer.Start()
	}

	if len(clusterCIDRs) != 0 || len(externalCIDRs) != 0 {
		// Create and start route generator, if configured to do so. This can either be through
		// environment variable, or the data store via BGPConfiguration.
		// We only turn it on if configured to do so, to avoid needing to watch services / endpoints.
		log.Info("Starting route generator for service advertisement")
		if c.rg, err = NewRouteGenerator(c); err != nil {
			log.WithError(err).Error("Failed to start route generator, routes will not be advertised")
			c.OnSyncChange(SourceRouteGenerator, true)
			c.rg = nil
		} else {
			c.rg.Start()
		}
	} else {
		c.OnSyncChange(SourceRouteGenerator, true)
	}

	// Start a goroutine to process updates in a way that's decoupled from their sources.
	go func() {
		for e := range c.syncerC {
			switch event := e.(type) {
			case []api.Update:
				c.onUpdates(event)
			case api.SyncStatus:
				c.onStatusUpdated(event)
			default:
				log.Panicf("Unknown type %T in syncer channel", event)
			}
		}
	}()

	return c, nil
}

var (
	SourceSyncer         string = "SourceSyncer"
	SourceRouteGenerator string = "SourceRouteGenerator"
)

// client implements the StoreClient interface for confd, and also implements the
// Calico api.SyncerCallbacks and api.SyncerParseFailCallbacks interfaces for the
// BGP Syncer.
type client struct {
	// The Calico backend client.
	client api.Client

	// The BGP syncer.
	syncer          api.Syncer
	nodeV1Processor watchersyncer.SyncerUpdateProcessor
	nodeLabels      map[string]map[string]string
	bgpPeers        map[string]*apiv3.BGPPeer

	// The route generator
	rg *routeGenerator

	// Readiness signals for individual data sources.
	sourceReady map[string]bool

	// Indicates whether all data sources have synced. We cannot start rendering until
	// all sources have synced, so we block calls to GetValues until this is true.
	syncedOnce  bool
	waitForSync sync.WaitGroup

	// Our internal cache of key/values, and our (internally defined) cache revision.
	cache         map[string]string
	peeringCache  map[string]string
	cacheRevision uint64

	// The current revision for each prefix.  A revision is updated when we have a sync
	// event that updates any keys with that prefix.
	revisionsByPrefix map[string]uint64

	// Lock used to synchronize access to any of the shared mutable data.
	cacheLock   sync.Mutex
	watcherCond *sync.Cond

	// Whether the node to node mesh is enabled or not.
	nodeMeshEnabled bool

	// This node's log level key.
	nodeLogKey string

	// Current values of <bgpconfig>.spec.serviceExternalIPs and
	// <bgpconfig>.spec.serviceClusterIPs.
	externalIPs    []string
	externalIPNets []*net.IPNet // same as externalIPs but parsed
	clusterCIDRs   []string

	// Channel used to decouple update and status processing.
	syncerC chan interface{}
}

// SetPrefixes is called from confd to notify this client of the full set of prefixes that will
// be watched.
// This client uses this information to initialize the revision map used to keep track of the
// revision number of each prefix that the template is monitoring.
func (c *client) SetPrefixes(keys []string) error {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	log.Debugf("Set prefixes called with: %v", keys)
	for _, k := range keys {
		// Initialise the revision that we are watching for this prefix.  This will be updated
		// if we receive any syncer events for keys with this prefix.  The Watcher function will
		// then check the revisions it is interested in to see if there is an updated revision
		// that it needs to process.
		c.revisionsByPrefix[k] = 0
	}

	return nil
}

// OnStatusUpdated is called from the BGP syncer to indicate that the sync status is updated.
// This client handles InSync and WaitForDatastore statuses. When we receive InSync, we unblock GetValues calls.
// When we receive WaitForDatastore and are already InSync, we reset the client's syncer status which blocks
// GetValues calls.
func (c *client) OnStatusUpdated(status api.SyncStatus) {
	c.syncerC <- status
}

func (c *client) onStatusUpdated(status api.SyncStatus) {
	log.Debugf("Got status update: %s", status)
	switch status {
	case api.InSync:
		c.OnSyncChange(SourceSyncer, true)
	case api.WaitForDatastore:
		c.OnSyncChange(SourceSyncer, false)
	}
}

// OnInSync handles multiplexing in-sync messages from multiple data sources
// into a single representation of readiness.
func (c *client) OnSyncChange(source string, ready bool) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	if ready == c.sourceReady[source] {
		log.Debugf("No change for source %v, ready %v", source, ready)
		return
	}

	log.Infof("Source %v readiness changed, ready=%v", source, ready)

	// Check if we are fully in sync, before applying this change.
	oldFullSync := c.sourceReady[SourceSyncer] && c.sourceReady[SourceRouteGenerator]

	// Apply the change.
	c.sourceReady[source] = ready

	// Check if we are fully in sync now.
	newFullSync := c.sourceReady[SourceSyncer] && c.sourceReady[SourceRouteGenerator]

	if newFullSync == oldFullSync {
		log.Debugf("No change to full sync status (%v)", newFullSync)
		return
	}

	if newFullSync {
		// All data sources are ready.
		c.syncedOnce = true
		c.waitForSync.Done()
		log.Info("Data is now syncd, can start rendering templates")

		// Now that we're in-sync, check if we should update our log level
		// based on the datastore config.
		c.updateLogLevel()
	} else {
		log.Info("Full sync lost")
		c.waitForSync.Add(1)
	}
}

type bgpPeer struct {
	PeerIP      cnet.IP              `json:"ip"`
	ASNum       numorstring.ASNumber `json:"as_num,string"`
	RRClusterID string               `json:"rr_cluster_id"`
}

func (c *client) updatePeersV1() {
	// A map that will contain the v1 peerings that should exist, with the same key and
	// value form as c.peeringCache.
	peersV1 := make(map[string]string)

	// Common subroutine for emitting both global and node-specific peerings.
	emit := func(key model.Key, peer *bgpPeer) {
		log.WithFields(log.Fields{"key": key, "peer": peer}).Debug("Maybe emit peering")

		// Compute etcd v1 path for this peering key.
		k, err := model.KeyToDefaultPath(key)
		if err != nil {
			log.Errorf("Ignoring update: unable to create path from Key %v: %v", key, err)
			return
		}

		// If we already have an entry for that path, it wins.  When we're
		// emitting reverse peerings to ensure symmetry, this is what ensures
		// that an explicit forwards peering is not overwritten by an implicit
		// reverse peering.
		if _, ok := peersV1[k]; ok {
			log.Debug("Peering already exists")
			return
		}

		// If we would be emitting a node-specific peering to a peer IP, and we
		// already have a global peering to that IP, skip emitting the node-specific
		// one.
		if nodeKey, ok := key.(model.NodeBGPPeerKey); ok {
			globalKey := model.GlobalBGPPeerKey{PeerIP: nodeKey.PeerIP}
			globalPath, _ := model.KeyToDefaultPath(globalKey)
			if _, ok = peersV1[globalPath]; ok {
				log.Debug("Global peering already exists")
				return
			}
		}

		// Serialize and store the value for this peering.
		value, err := json.Marshal(peer)
		if err != nil {
			log.Errorf("Ignoring update: unable to serialize value %v: %v", peer, err)
			return
		}
		peersV1[k] = string(value)
	}

	// Loop through v3 BGPPeers twice, first to emit global peerings, then for
	// node-specific ones.  The point here is to emit all of the possible global peerings
	// _first_, so that we can then skip emitting any node-specific peerings that would
	// duplicate those on particular nodes.
	for _, globalPass := range []bool{true, false} {
		for _, v3res := range c.bgpPeers {
			log.WithField("peer", v3res).Debug("Process v3 BGPPeer")
			if globalPass != ((v3res.Spec.NodeSelector == "") && (v3res.Spec.Node == "")) {
				log.WithField("globalPass", globalPass).Debug("Skip BGPPeer on this pass")
				continue
			}

			var localNodeNames []string
			if v3res.Spec.NodeSelector != "" {
				localNodeNames = c.nodesMatching(v3res.Spec.NodeSelector)
			} else if v3res.Spec.Node != "" {
				localNodeNames = []string{v3res.Spec.Node}
			}
			log.Debugf("Local nodes %#v", localNodeNames)

			var peers []*bgpPeer
			if v3res.Spec.PeerSelector != "" {
				for _, peerNodeName := range c.nodesMatching(v3res.Spec.PeerSelector) {
					peers = append(peers, c.nodeAsBGPPeers(peerNodeName)...)
				}
			} else {
				ip := cnet.ParseIP(v3res.Spec.PeerIP)
				if ip == nil {
					log.Warning("PeerIP is not assigned or is malformed")
					continue
				}
				peers = append(peers, &bgpPeer{
					PeerIP: *ip,
					ASNum:  v3res.Spec.ASNumber,
				})
			}
			log.Debugf("Peers %#v", peers)

			for _, peer := range peers {
				log.Debugf("Peer: %#v", peer)
				if globalPass {
					key := model.GlobalBGPPeerKey{PeerIP: peer.PeerIP}
					emit(key, peer)
				} else {
					for _, localNodeName := range localNodeNames {
						log.Debugf("Local node name: %#v", localNodeName)
						key := model.NodeBGPPeerKey{Nodename: localNodeName, PeerIP: peer.PeerIP}
						emit(key, peer)
					}
				}
			}
		}
	}

	// Loop through v3 BGPPeers again to add in any missing reverse peerings.
	for _, v3res := range c.bgpPeers {
		log.WithField("peer", v3res).Debug("Second pass with v3 BGPPeer")

		// This time, the "local" nodes are actually those matching the remote fields
		// in BGPPeer, i.e. PeerIP, ASNumber and PeerSelector...
		var localNodeNames []string
		if v3res.Spec.PeerSelector != "" {
			localNodeNames = c.nodesMatching(v3res.Spec.PeerSelector)
		} else {
			localNodeNames = c.nodesWithIPAndAS(v3res.Spec.PeerIP, v3res.Spec.ASNumber)
		}
		log.Debugf("Local nodes %#v", localNodeNames)

		// Skip peer computation if there are no local nodes.
		if len(localNodeNames) == 0 {
			continue
		}

		// ...and the "peer" nodes are those matching the local fields in BGPPeer, i.e
		// Node and NodeSelector.
		var peerNodeNames []string
		if v3res.Spec.NodeSelector != "" {
			peerNodeNames = c.nodesMatching(v3res.Spec.NodeSelector)
		} else if v3res.Spec.Node != "" {
			peerNodeNames = []string{v3res.Spec.Node}
		} else {
			peerNodeNames = c.nodesMatching("all()")
		}
		log.Debugf("Peers %#v", peerNodeNames)

		for _, peerNodeName := range peerNodeNames {
			for _, peer := range c.nodeAsBGPPeers(peerNodeName) {
				for _, localNodeName := range localNodeNames {
					key := model.NodeBGPPeerKey{Nodename: localNodeName, PeerIP: peer.PeerIP}
					emit(key, peer)
				}
			}
		}
	}

	// Now reconcile against the cache.
	for k, value := range c.peeringCache {
		newValue, ok := peersV1[k]
		if !ok {
			// This cache entry should be deleted.
			delete(c.peeringCache, k)
			c.keyUpdated(k)
		} else if newValue != value {
			// This cache entry should be updated.
			c.peeringCache[k] = newValue
			c.keyUpdated(k)
			delete(peersV1, k)
		} else {
			// Value in cache is already correct.  Delete from peersV1 so that we
			// don't generate a spurious keyUpdated for this key.
			delete(peersV1, k)
		}
	}
	// peersV1 now only contains peerings to add to the cache.
	for k, newValue := range peersV1 {
		c.peeringCache[k] = newValue
		c.keyUpdated(k)
	}
}

func (c *client) nodesMatching(rawSelector string) []string {
	nodeNames := []string{}
	sel, err := selector.Parse(rawSelector)
	if err != nil {
		log.Errorf("Couldn't parse selector: %v", rawSelector)
		return nodeNames
	}
	for nodeName, labels := range c.nodeLabels {
		if sel.Evaluate(labels) {
			nodeNames = append(nodeNames, nodeName)
		}
	}
	return nodeNames
}

func (c *client) nodesWithIPAndAS(ip string, asNum numorstring.ASNumber) []string {
	globalAS := c.globalAS()
	var asStr string
	if asNum == numorstring.ASNumber(0) {
		asStr = globalAS
	} else {
		asStr = asNum.String()
	}
	nodeNames := []string{}
	for nodeName := range c.nodeLabels {
		nodeIPv4, nodeIPv6, nodeAS, _ := c.nodeToBGPFields(nodeName)
		if (nodeIPv4 != ip) && (nodeIPv6 != ip) {
			continue
		}
		if nodeAS == "" {
			nodeAS = globalAS
		}
		if nodeAS != asStr {
			continue
		}
		nodeNames = append(nodeNames, nodeName)
	}
	return nodeNames
}

func (c *client) nodeToBGPFields(nodeName string) (string, string, string, string) {
	ipv4Key, _ := model.KeyToDefaultPath(model.NodeBGPConfigKey{Nodename: nodeName, Name: "ip_addr_v4"})
	ipv6Key, _ := model.KeyToDefaultPath(model.NodeBGPConfigKey{Nodename: nodeName, Name: "ip_addr_v6"})
	asKey, _ := model.KeyToDefaultPath(model.NodeBGPConfigKey{Nodename: nodeName, Name: "as_num"})
	rrKey, _ := model.KeyToDefaultPath(model.NodeBGPConfigKey{Nodename: nodeName, Name: "rr_cluster_id"})
	return c.cache[ipv4Key], c.cache[ipv6Key], c.cache[asKey], c.cache[rrKey]
}

func (c *client) globalAS() string {
	asKey, _ := model.KeyToDefaultPath(model.GlobalBGPConfigKey{Name: "as_num"})
	return c.cache[asKey]
}

func (c *client) nodeAsBGPPeers(nodeName string) (peers []*bgpPeer) {
	ipv4Str, ipv6Str, asNum, rrClusterID := c.nodeToBGPFields(nodeName)
	for version, ipStr := range map[string]string{
		"IPv4": ipv4Str,
		"IPv6": ipv6Str,
	} {
		peer := &bgpPeer{}
		if ipStr == "" {
			log.Debugf("No %v for node %v", version, nodeName)
			continue
		}
		ip := cnet.ParseIP(ipStr)
		if ip == nil {
			log.Warningf("Couldn't parse %v %v for node %v", version, ipStr, nodeName)
			continue
		}
		peer.PeerIP = *ip
		var err error
		if asNum != "" {
			log.Debugf("ASNum for %v is %#v", nodeName, asNum)
			peer.ASNum, err = numorstring.ASNumberFromString(asNum)
			if err != nil {
				log.WithError(err).Warningf("Problem parsing AS number %v for node %v", asNum, nodeName)
			}
		} else {
			asNum = c.globalAS()
			log.Debugf("Global ASNum for %v is %#v", nodeName, asNum)
			peer.ASNum, err = numorstring.ASNumberFromString(asNum)
			if err != nil {
				log.WithError(err).Warningf("Problem parsing global AS number %v for node %v", asNum, nodeName)
			}
		}
		peer.RRClusterID = rrClusterID
		peers = append(peers, peer)
	}
	return
}

// OnUpdates is called from the BGP syncer to indicate that new updates are available from the
// Calico datastore.
// This client does the following:
// -  stores the updates in its local cache
// -  increments the revision number associated with each of the affected watch prefixes
// -  wakes up the watchers so that they can check if any of the prefixes they are
//    watching have been updated.
func (c *client) OnUpdates(updates []api.Update) {
	c.syncerC <- updates
}

func (c *client) onUpdates(updates []api.Update) {

	// Update our cache from the updates.
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	// Indicate that our cache has been updated.
	c.incrementCacheRevision()

	// Track whether these updates require BGP peerings to be recomputed.
	needUpdatePeersV1 := false
	needUpdatePeersReasons := []string{}

	// Track whether these updates require service advertisement to be recomputed.
	needServiceAdvertismentUpdates := false

	log.WithField("cacheRevision", c.cacheRevision).Debug("Processing OnUpdates from syncer")
	for _, u := range updates {
		log.Debugf("Update: %#v", u)

		// confd now receives Nodes and BGPPeers as v3 resources.
		//
		// For each Node, we save off the node's labels, then convert to v1 so that
		// the same etcd key/value pairs appear as before (so that existing confd
		// templates will continue to work).
		//
		// BGPPeers are saved off and then the whole set is processed to generate a
		// corresponding set of v1 BGPPeers, bearing in mind (a) the possible use of
		// v3 BGPPeer selector fields, and (b) that we fill in any reverse peerings
		// that are needed for symmetry between Calico nodes.  Each v1 BGPPeer then
		// generates etcd key/value pairs as expected by existing confd templates.
		v3key, ok := u.Key.(model.ResourceKey)
		if !ok {
			// Not a v3 resource. We care about when the BGP configuration changes - recalculate
			// peers when we receive AS number updates.
			if cfgKey, ok := u.Key.(model.GlobalBGPConfigKey); ok {
				if cfgKey.Name == "as_num" {
					log.Debugf("Global AS number update, need to recalculate peers")
					needUpdatePeersV1 = true
					needUpdatePeersReasons = append(needUpdatePeersReasons, "Global AS number changed")
				}

				if cfgKey.Name == "svc_external_ips" {
					log.Debugf("Global service external IP ranges update.")
					needServiceAdvertismentUpdates = true
				}

				if cfgKey.Name == "svc_cluster_ips" {
					log.Debugf("Global service cluster IP ranges update.")
					if len(os.Getenv(envAdvertiseClusterIPs)) != 0 {
						// ClusterIPs are configurable through an environment variable. If specified,
						// that variable takes precedence over datastore config, so we should ignore the update.
						log.Debugf("Ignoring serviceClusterIPs update due to environment variable %s", envAdvertiseClusterIPs)
					} else {
						needServiceAdvertismentUpdates = true
					}
				}
			}

			if cfgKey, ok := u.Key.(model.NodeBGPConfigKey); ok {
				if cfgKey.Name == "as_num" {
					log.WithField("node", cfgKey.Nodename).Debugf("Node AS number update, need to recalculate peers")
					needUpdatePeersV1 = true
					needUpdatePeersReasons = append(needUpdatePeersReasons, "Node AS number changed")
				}
			}
			continue
		}

		// It's a v3 resource - we care about some of these.
		if v3key.Kind == apiv3.KindNode {
			// Convert to v1 key/value pairs.
			log.Debugf("Node: %#v", u.Value)
			if u.Value != nil {
				log.Debugf("BGPSpec: %#v", u.Value.(*apiv3.Node).Spec.BGP)
			}
			kvps, err := c.nodeV1Processor.Process(&u.KVPair)
			if err != nil {
				log.Errorf("Problem converting Node resource: %v", err)
				continue
			}
			for _, kvp := range kvps {
				log.Debugf("KVP: %#v", kvp)
				if kvp.Value == nil {
					if c.updateCache(api.UpdateTypeKVDeleted, kvp) {
						needUpdatePeersV1 = true
						needUpdatePeersReasons = append(needUpdatePeersReasons, fmt.Sprintf("%s deleted", kvp.Key.String()))
					}
				} else {
					if c.updateCache(u.UpdateType, kvp) {
						needUpdatePeersV1 = true
						needUpdatePeersReasons = append(needUpdatePeersReasons, fmt.Sprintf("%s updated", kvp.Key.String()))
					}
				}
			}

			// Update our cache of node labels.
			if u.Value == nil {
				// This was a delete - remove node labels.
				if _, ok := c.nodeLabels[v3key.Name]; ok {
					delete(c.nodeLabels, v3key.Name)
					needUpdatePeersV1 = true
					needUpdatePeersReasons = append(needUpdatePeersReasons, v3key.Name+" deleted")
				}
			} else {
				// This was a create or update - update node labels.
				v3res, ok := u.Value.(*apiv3.Node)
				if !ok {
					log.Warning("Bad value for Node resource")
					continue
				}
				existingLabels, isSet := c.nodeLabels[v3key.Name]
				if !isSet || !reflect.DeepEqual(existingLabels, v3res.Labels) {
					c.nodeLabels[v3key.Name] = v3res.Labels
					needUpdatePeersV1 = true
					needUpdatePeersReasons = append(needUpdatePeersReasons, v3key.Name+" updated")
				}
			}
		}

		if v3key.Kind == apiv3.KindBGPPeer {
			// Update our cache of v3 BGPPeer resources.
			if u.Value == nil || u.UpdateType == api.UpdateTypeKVDeleted {
				delete(c.bgpPeers, v3key.Name)
			} else if v3res, ok := u.Value.(*apiv3.BGPPeer); ok {
				c.bgpPeers[v3key.Name] = v3res
			} else {
				log.Warning("Bad value for BGPPeer resource")
				continue
			}

			// Note need to recompute equivalent v1 peerings.
			needUpdatePeersV1 = true
			needUpdatePeersReasons = append(needUpdatePeersReasons, "BGP peer updated or deleted")
		}
	}

	// Update our cache from each of the individual updates, and keep track of
	// any of the prefixes that are impacted.
	for _, u := range updates {
		c.updateCache(u.UpdateType, &u.KVPair)
	}

	// If configuration relevant to BGP peerings has changed, recalculate the set of v1
	// peerings that should exist, and update the cache accordingly.
	if needUpdatePeersV1 {
		log.Info("Recompute BGP peerings: " + strings.Join(needUpdatePeersReasons, "; "))
		c.updatePeersV1()
	}

	// If we need to update Service advertisement based on the updates, then do so.
	if needServiceAdvertismentUpdates {
		log.Info("Updates included service advertisment changes.")
		if c.rg == nil {
			// If this is the first time we've needed to start the route generator, then do so here.
			log.Info("Starting route generator due to service advertisement update")
			var err error
			if c.rg, err = NewRouteGenerator(c); err != nil {
				log.WithError(err).Error("Failed to start route generator, unable to advertise node-specific service routes")
				c.rg = nil
			} else {
				c.rg.Start()
			}
		}

		// Update external IP CIDRs. In v1 format, they are a single comma-separated
		// string. If the string isn't empty, split on the comma and pass a list of strings
		// to the route generator.  An empty string indicates a withdrawal of that set of
		// service IPs.
		var externalIPs []string
		if len(c.cache["/calico/bgp/v1/global/svc_external_ips"]) > 0 {
			externalIPs = strings.Split(c.cache["/calico/bgp/v1/global/svc_external_ips"], ",")
		}
		c.onExternalIPsUpdate(externalIPs)

		// Same for cluster CIDRs.
		var clusterIPs []string
		if len(c.cache["/calico/bgp/v1/global/svc_cluster_ips"]) > 0 {
			clusterIPs = strings.Split(c.cache["/calico/bgp/v1/global/svc_cluster_ips"], ",")
		}
		c.onClusterIPsUpdate(clusterIPs)

		if c.rg != nil {
			// Trigger the route generator to recheck and advertise or withdraw
			// node-specific routes.
			c.rg.TriggerResync()
		}
	}

	// Notify watcher thread that we've received new updates.
	log.WithField("cacheRevision", c.cacheRevision).Debug("Done processing OnUpdates from syncer, notify watchers")
	c.onNewUpdates()
}

func (c *client) onExternalIPsUpdate(externalIPs []string) {
	if err := c.updateGlobalRoutes(c.externalIPs, externalIPs); err == nil {
		c.externalIPs = externalIPs
		c.externalIPNets = parseIPNets(c.externalIPs)
		log.Infof("Updated with new external IP CIDRs: %s", externalIPs)
	} else {
		log.WithError(err).Error("Failed to update external IP routes")
	}
}

func (c *client) onClusterIPsUpdate(clusterCIDRs []string) {
	if err := c.updateGlobalRoutes(c.clusterCIDRs, clusterCIDRs); err == nil {
		c.clusterCIDRs = clusterCIDRs
		log.Infof("Updated with new cluster IP CIDRs: %s", clusterCIDRs)
	} else {
		log.WithError(err).Error("Failed to update cluster CIDR routes")
	}
}

func (c *client) AdvertiseClusterIPs() bool {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	return len(c.clusterCIDRs) > 0
}

func (c *client) GetExternalIPs() []*net.IPNet {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	return c.externalIPNets
}

// "Global" here means the routes for cluster IP and external IP CIDRs that are advertised from
// every node in the cluster.
func (c *client) updateGlobalRoutes(current, new []string) error {
	for _, n := range new {
		_, _, err := net.ParseCIDR(n)
		if err != nil {
			// Shouldn't ever happen, given prior validation.
			return err
		}
	}

	// Find any currently advertised CIDRs that we should withdraw.
	withdraws := []string{}
	for _, existing := range current {
		if !contains(new, existing) {
			withdraws = append(withdraws, existing)
		}
	}

	// Withdraw the old CIDRs and add the new.
	c.addRoutesLockHeld(rejectKeyPrefix, rejectKeyPrefixV6, new)
	c.addRoutesLockHeld(routeKeyPrefix, routeKeyPrefixV6, new)
	c.deleteRoutesLockHeld(rejectKeyPrefix, rejectKeyPrefixV6, withdraws)
	c.deleteRoutesLockHeld(routeKeyPrefix, routeKeyPrefixV6, withdraws)

	return nil
}

func (c *client) incrementCacheRevision() {
	// If we are in-sync then this is an incremental update, so increment our internal
	// cache revision.
	if c.syncedOnce {
		c.cacheRevision++
		log.Debugf("Processing new updates, revision is now: %d", c.cacheRevision)
	}
}

func (c *client) onNewUpdates() {
	if c.syncedOnce {
		// Wake up the watchers to let them know there may be some updates of interest.  We only
		// need to do this once we're synced because until that point all of the Watcher threads
		// will be blocked getting values.
		log.Debug("Notify watchers of new event data")
		c.watcherCond.Broadcast()
	}
}

// updateChache will update a cache entry. It returns true if the entry was
// updated and false if there was an error or if the cache was already
// up-to-date.
func (c *client) updateCache(updateType api.UpdateType, kvp *model.KVPair) bool {
	// Update our cache of current entries.
	k, err := model.KeyToDefaultPath(kvp.Key)
	if err != nil {
		log.Errorf("Ignoring update: unable to create path from Key %v: %v", kvp.Key, err)
		return false
	}

	switch updateType {
	case api.UpdateTypeKVDeleted:
		// The bird templates that confd is used to render assume that some global
		// defaults are always configured.
		if globalDefault, ok := globalDefaults[k]; ok {
			if currentValue, hasKey := c.cache[k]; hasKey && currentValue == globalDefault {
				return false
			}
			c.cache[k] = globalDefault
		} else {
			if _, hasValue := c.cache[k]; !hasValue {
				return false
			}
			delete(c.cache, k)
		}
	case api.UpdateTypeKVNew, api.UpdateTypeKVUpdated:
		value, err := model.SerializeValue(kvp)
		if err != nil {
			log.Errorf("Ignoring update: unable to serialize value %v: %v", kvp.Value, err)
			return false
		}
		newValue := string(value)
		if currentValue, isSet := c.cache[k]; isSet && currentValue == newValue {
			return false
		}
		c.cache[k] = newValue
	}

	log.Debugf("Cache entry updated from event type %d: %s=%s", updateType, k, c.cache[k])
	if c.syncedOnce {
		c.keyUpdated(k)
	}
	return true
}

// ParseFailed is called from the BGP syncer when an event could not be parsed.
// We use this purely for logging.
func (c *client) ParseFailed(rawKey string, rawValue string) {
	log.Errorf("Unable to parse datastore entry Key=%s; Value=%s", rawKey, rawValue)
}

// GetValues is called from confd to obtain the cached data for the required set of prefixes.
// We simply populate the values from our caches, only returning values which have the
// requested set of prefixes.
func (c *client) GetValues(keys []string) (map[string]string, error) {
	// We should block GetValues until we have the sync'd notification - until that point we
	// only have a partial snapshot and we should never write out partial config.
	c.waitForSync.Wait()

	log.Debugf("Requesting values for keys: %v", keys)

	// Lock the data and then populate the results from our caches, selecting the data
	// whose path matches the set of prefix keys.
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	values := map[string]string{}
	for k, v := range c.cache {
		if c.matchesPrefix(k, keys) {
			values[k] = v
		}
	}
	for k, v := range c.peeringCache {
		if c.matchesPrefix(k, keys) {
			values[k] = v
		}
	}

	log.Debugf("Returning %d results", len(values))

	return values, nil
}

// WatchPrefix is called from confd.  It blocks waiting for updates to the data which have any
// of the requested set of prefixes.
//
// Since we keep track of revisions per prefix, all we need to do is check the revisions for an
// update, and if there is no update we wait on the conditional which is woken by the OnUpdates
// thread after updating the cache.  If any of the watched revisions is greater than the waitIndex
// then exit to render.
func (c *client) WatchPrefix(prefix string, keys []string, lastRevision uint64, stopChan chan bool) (string, error) {
	log.WithFields(log.Fields{"prefix": prefix, "keys": keys}).Debug("WatchPrefix entry")
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	if lastRevision == 0 {
		// If this is the first iteration, we always exit to ensure we render with the initial
		// synced settings.
		log.Debug("First watch call for template - exiting to render template")
		return "", nil
	}

	for {
		// Loop through each key, if the revision associated with the key is higher than the lastRevision
		// then exit with the current cacheRevision and render with the current data.
		log.Debugf("Checking for updated key revisions, watching from rev %d", lastRevision)
		for _, key := range keys {
			rev, ok := c.revisionsByPrefix[key]
			if !ok {
				log.Fatalf("Watch prefix check for unknown prefix: %s", key)
			}
			log.Debugf("Found key prefix %s at rev %d", key, rev)
			if rev > lastRevision {
				log.Debug("Exiting to render template")
				return key, nil
			}
		}

		// No changes for this watcher, so wait until there are more syncer events.
		log.Debug("No updated keys for this template - waiting for event notification")
		c.watcherCond.Wait()
		log.WithFields(log.Fields{"prefix": prefix, "keys": keys}).Debug("WatchPrefix recheck")
	}
}

// GetCurrentRevision returns the current revision of the data in our cache.
func (c *client) GetCurrentRevision() uint64 {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()
	log.Debugf("Current cache revision is %v", c.cacheRevision)
	return c.cacheRevision
}

// matchesPrefix returns true if the key matches any of the supplied prefixes.
func (c *client) matchesPrefix(key string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// Called when a key is updated.  This updates the revision associated with key prefixes
// affected by this key.
// The caller should be holding the cacheLock.
func (c *client) keyUpdated(key string) {
	for prefix, rev := range c.revisionsByPrefix {
		log.Debugf("Prefix %s has rev %d", prefix, rev)
		if rev != c.cacheRevision && strings.HasPrefix(key, prefix) {
			log.Debugf("Updating prefix to rev %d", c.cacheRevision)
			c.revisionsByPrefix[prefix] = c.cacheRevision

			// If this is a change to either the global log level, or the per-node
			// log level, then configure confd's log level to match.
			if strings.HasSuffix(key, "loglevel") {
				log.WithField("key", key).Info("Potential log level configuration change on key")
				c.updateLogLevel()
			}
		}
	}
}

func (c *client) updateLogLevel() {
	if dcls := os.Getenv(DEBUGGING_CONFIGURATION_LOG_LEVEL); dcls != "" {
		logutils.SetLevel("debug")
	} else {
		if envLevel := os.Getenv("BGP_LOGSEVERITYSCREEN"); envLevel != "" {
			logutils.SetLevel(envLevel)
		} else if nodeLevel := c.cache[c.nodeLogKey]; nodeLevel != "" {
			logutils.SetLevel(nodeLevel)
		} else if globalLogLevel := c.cache[globalLogging]; globalLogLevel != "" {
			logutils.SetLevel(globalLogLevel)
		} else {
			logutils.SetLevel("info")
		}
	}
}

var routeKeyPrefix = "/calico/staticroutes/"
var rejectKeyPrefix = "/calico/rejectcidrs/"
var routeKeyPrefixV6 = "/calico/staticroutesv6/"
var rejectKeyPrefixV6 = "/calico/rejectcidrsv6/"

func (c *client) addRoutesLockHeld(prefixV4, prefixV6 string, cidrs []string) {
	for _, cidr := range cidrs {
		var k string
		if strings.Contains(cidr, ":") {
			k = prefixV6 + strings.Replace(cidr, "/", "-", 1)
		} else {
			k = prefixV4 + strings.Replace(cidr, "/", "-", 1)
		}
		c.cache[k] = cidr
		c.keyUpdated(k)
	}
}

func (c *client) deleteRoutesLockHeld(prefixV4, prefixV6 string, cidrs []string) {
	for _, cidr := range cidrs {
		var k string
		if strings.Contains(cidr, ":") {
			k = prefixV6 + strings.Replace(cidr, "/", "-", 1)
		} else {
			k = prefixV4 + strings.Replace(cidr, "/", "-", 1)
		}
		delete(c.cache, k)
		c.keyUpdated(k)
	}
}

// AddStaticRoutes adds the given CIDRs as static routes to be advertised from this node.
func (c *client) AddStaticRoutes(cidrs []string) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	c.incrementCacheRevision()
	c.addRoutesLockHeld(routeKeyPrefix, routeKeyPrefixV6, cidrs)
	c.onNewUpdates()
}

// DeleteStaticRoutes withdraws the given CIDRs from the set of static routes advertised
// from this node.
func (c *client) DeleteStaticRoutes(cidrs []string) {
	c.cacheLock.Lock()
	defer c.cacheLock.Unlock()

	c.incrementCacheRevision()
	c.deleteRoutesLockHeld(routeKeyPrefix, routeKeyPrefixV6, cidrs)
	c.onNewUpdates()
}
