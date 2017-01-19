// Copyright (c) 2016-2017 Tigera, Inc. All rights reserved.
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

package intdataplane

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/projectcalico/felix/go/felix/ifacemonitor"
	"github.com/projectcalico/felix/go/felix/ip"
	"github.com/projectcalico/felix/go/felix/iptables"
	"github.com/projectcalico/felix/go/felix/proto"
	"github.com/projectcalico/felix/go/felix/routetable"
	"github.com/projectcalico/felix/go/felix/rules"
	"github.com/projectcalico/felix/go/felix/set"
	"io"
	"net"
	"os"
	"reflect"
	"regexp"
	"strings"
)

type routeTable interface {
	SetRoutes(ifaceName string, targets []routetable.Target)
}

// endpointManager manages the dataplane resources that belong to each endpoint as well as
// the "dispatch chains" that fan out packets to the right per-endpoint chain.
//
// It programs the relevant iptables chains (via the iptables.Table objects) along with
// per-endpoint routes (via the RouteTable).
//
// Since calculating the dispatch chains is fairly expensive, the main OnUpdate method
// simply records the pending state of each interface and defers the actual calculation
// to CompleteDeferredWork().  This is also the basis of our failure handling; updates
// that fail are left in the pending state so they can be retried later.
type endpointManager struct {
	// Config.
	ipVersion      uint8
	wlIfacesRegexp *regexp.Regexp

	// Our dependencies.
	filterTable  iptablesTable
	ruleRenderer rules.RuleRenderer
	routeTable   routeTable
	writeProcSys procSysWriter

	// Pending updates, cleared in CompleteDeferredWork as the data is copied to the activeXYZ
	// fields.
	pendingWlEpUpdates  map[proto.WorkloadEndpointID]*proto.WorkloadEndpoint
	pendingIfaceUpdates map[string]ifacemonitor.State

	// Active state, updated in CompleteDeferredWork.
	activeWlEndpoints     map[proto.WorkloadEndpointID]*proto.WorkloadEndpoint
	activeWlIfaceNameToID map[string]proto.WorkloadEndpointID
	activeUpIfaces        set.Set
	activeWlIDToChains    map[proto.WorkloadEndpointID][]*iptables.Chain
	activeDispatchChains  []*iptables.Chain

	// wlIfaceNamesToReconfigure contains names of workload interfaces that need to have
	// their configuration (sysctls etc.) refreshed.
	wlIfaceNamesToReconfigure set.Set

	// epIDsToUpdateStatus contains IDs of endpoints that we need to report status for.
	// Mix of host and workload endpoint IDs.
	epIDsToUpdateStatus set.Set

	// hostIfaceToAddrs maps host interface name to the set of IPs on that interface (reported
	// fro the dataplane).
	hostIfaceToAddrs map[string]set.Set
	// rawHostEndpoints contains the raw (i.e. not resolved to interface) host endpoints.
	rawHostEndpoints map[proto.HostEndpointID]*proto.HostEndpoint
	// hostEndpointsDirty is set to true when host endpoints are updated.
	hostEndpointsDirty bool
	// activeHostIfaceToChains maps host interface name to the chains that we've programmed.
	activeHostIfaceToChains map[string][]*iptables.Chain
	// activeHostDispatchChains contains the dispatch chains that we've programmed for host
	// endpoints.
	activeHostDispatchChains []*iptables.Chain
	// activeHostEpIDToIfaceName records which interface we resolved each host endpoint to.
	activeHostEpIDToIfaceName map[proto.HostEndpointID]string
	// activeIfaceNameToHostEpID records which endpoint we resolved each host interface to.
	activeIfaceNameToHostEpID map[string]proto.HostEndpointID

	// Callbacks
	OnEndpointStatusUpdate EndpointStatusUpdateCallback
}

type EndpointStatusUpdateCallback func(ipVersion uint8, id interface{}, status string)

type procSysWriter func(path, value string) error

func newEndpointManager(
	filterTable iptablesTable,
	ruleRenderer rules.RuleRenderer,
	routeTable routeTable,
	ipVersion uint8,
	wlInterfacePrefixes []string,
	onWorkloadEndpointStatusUpdate EndpointStatusUpdateCallback,
) *endpointManager {
	return newEndpointManagerWithShims(
		filterTable,
		ruleRenderer,
		routeTable,
		ipVersion,
		wlInterfacePrefixes,
		onWorkloadEndpointStatusUpdate,
		writeProcSys,
	)
}

func newEndpointManagerWithShims(
	filterTable iptablesTable,
	ruleRenderer rules.RuleRenderer,
	routeTable routeTable,
	ipVersion uint8,
	wlInterfacePrefixes []string,
	onWorkloadEndpointStatusUpdate EndpointStatusUpdateCallback,
	procSysWriter procSysWriter,
) *endpointManager {
	wlIfacesPattern := "^(" + strings.Join(wlInterfacePrefixes, "|") + ").*"
	wlIfacesRegexp := regexp.MustCompile(wlIfacesPattern)

	return &endpointManager{
		ipVersion:      ipVersion,
		wlIfacesRegexp: wlIfacesRegexp,

		filterTable:  filterTable,
		ruleRenderer: ruleRenderer,
		routeTable:   routeTable,
		writeProcSys: procSysWriter,

		// Pending updates, we store these up as OnUpdate is called, then process them
		// in CompleteDeferredWork and transfer the important data to the activeXYX fields.
		pendingWlEpUpdates:  map[proto.WorkloadEndpointID]*proto.WorkloadEndpoint{},
		pendingIfaceUpdates: map[string]ifacemonitor.State{},

		activeUpIfaces: set.New(),

		activeWlEndpoints:     map[proto.WorkloadEndpointID]*proto.WorkloadEndpoint{},
		activeWlIfaceNameToID: map[string]proto.WorkloadEndpointID{},
		activeWlIDToChains:    map[proto.WorkloadEndpointID][]*iptables.Chain{},

		wlIfaceNamesToReconfigure: set.New(),

		epIDsToUpdateStatus: set.New(),

		hostIfaceToAddrs:   map[string]set.Set{},
		rawHostEndpoints:   map[proto.HostEndpointID]*proto.HostEndpoint{},
		hostEndpointsDirty: true,

		activeHostIfaceToChains:  map[string][]*iptables.Chain{},
		activeHostDispatchChains: nil,

		OnEndpointStatusUpdate: onWorkloadEndpointStatusUpdate,
	}
}

func (m *endpointManager) OnUpdate(protoBufMsg interface{}) {
	switch msg := protoBufMsg.(type) {
	case *proto.WorkloadEndpointUpdate:
		m.pendingWlEpUpdates[*msg.Id] = msg.Endpoint
	case *proto.WorkloadEndpointRemove:
		m.pendingWlEpUpdates[*msg.Id] = nil
	case *proto.HostEndpointUpdate:
		log.WithField("msg", msg).Debug("Host endpoint update")
		m.rawHostEndpoints[*msg.Id] = msg.Endpoint
		m.hostEndpointsDirty = true
	case *proto.HostEndpointRemove:
		log.WithField("msg", msg).Debug("Host endpoint removed")
		delete(m.rawHostEndpoints, *msg.Id)
		m.hostEndpointsDirty = true
	case *ifaceUpdate:
		log.WithField("update", msg).Debug("Interface state changed.")
		m.pendingIfaceUpdates[msg.Name] = msg.State
	case *ifaceAddrsUpdate:
		log.WithField("update", msg).Debug("Interface addrs changed.")
		if m.wlIfacesRegexp.MatchString(msg.Name) {
			log.WithField("update", msg).Debug("Workload interface, ignoring.")
			return
		}
		if msg.Addrs != nil {
			m.hostIfaceToAddrs[msg.Name] = msg.Addrs
		} else {
			delete(m.hostIfaceToAddrs, msg.Name)
		}
		m.hostEndpointsDirty = true
	}
}

func (m *endpointManager) CompleteDeferredWork() error {
	// Copy the pending interface state to the active set and mark any interfaces that have
	// changed state for reconfiguration by resolveWorkload/HostEndpoints()
	for ifaceName, state := range m.pendingIfaceUpdates {
		if state == ifacemonitor.StateUp {
			m.activeUpIfaces.Add(ifaceName)
			if m.wlIfacesRegexp.MatchString(ifaceName) {
				log.WithField("ifaceName", ifaceName).Info(
					"Workload interface came up, marking for reconfiguration.")
				m.wlIfaceNamesToReconfigure.Add(ifaceName)
			}
		} else {
			m.activeUpIfaces.Discard(ifaceName)
		}
		// If this interface is linked to any already-existing endpoints, mark the endpoint
		// status for recalculation.
		m.markEndpointStatusDirtyByIface(ifaceName)
	}

	if err := m.resolveWorkloadEndpoints(); err != nil {
		log.WithError(err).Warn("Failed to resolve workload endpoints")
		return err
	}

	if m.hostEndpointsDirty {
		if err := m.resolveHostEndpoints(); err != nil {
			log.WithError(err).Warn("Failed to resolve host endpoints")
			return err
		}
		m.hostEndpointsDirty = false
	}

	// Since the pairings of endpoints and interfaces may have changed, rescan the interface
	// state updates and mark endpoint statuses for update.
	for ifaceName := range m.pendingIfaceUpdates {
		m.markEndpointStatusDirtyByIface(ifaceName)
		// Clean up as we go...
		delete(m.pendingIfaceUpdates, ifaceName)
	}

	// Now send any endpoint status updates.
	m.updateEndpointStatuses()

	return nil
}

func (m *endpointManager) markEndpointStatusDirtyByIface(ifaceName string) {
	logCxt := log.WithField("ifaceName", ifaceName)
	if epID, ok := m.activeWlIfaceNameToID[ifaceName]; ok {
		logCxt.Info("Workload interface state changed; marking for status update.")
		m.epIDsToUpdateStatus.Add(epID)
	} else if epID, ok := m.activeIfaceNameToHostEpID[ifaceName]; ok {
		logCxt.Info("Host interface state changed; marking for status update.")
		m.epIDsToUpdateStatus.Add(epID)
	} else {
		// We don't know about this interface yet (or it's already been deleted).
		// If the endpoint gets created, we'll do the update then. If it's been
		// deleted, we've already cleaned it up.
		logCxt.Debug("Ignoring interface state change for unknown interface.")
	}
}

func (m *endpointManager) updateEndpointStatuses() {
	m.epIDsToUpdateStatus.Iter(func(item interface{}) error {
		switch id := item.(type) {
		case proto.WorkloadEndpointID:
			status := m.calculateWorkloadEndpointStatus(id)
			m.OnEndpointStatusUpdate(m.ipVersion, id, status)
		case proto.HostEndpointID:
			status := m.calculateHostEndpointStatus(id)
			m.OnEndpointStatusUpdate(m.ipVersion, id, status)
		}

		return set.RemoveItem
	})
}

func (m *endpointManager) calculateWorkloadEndpointStatus(id proto.WorkloadEndpointID) string {
	logCxt := log.WithField("workloadEndpointID", id)
	logCxt.Debug("Re-evaluating workload endpoint status")
	var operUp, adminUp, failed bool
	workload, known := m.activeWlEndpoints[id]
	if known {
		adminUp = workload.State == "active"
		operUp = m.activeUpIfaces.Contains(workload.Name)
		failed = m.wlIfaceNamesToReconfigure.Contains(workload.Name)
	}

	var status string
	if known {
		if failed {
			status = "error"
		} else if operUp && adminUp {
			status = "up"
		} else {
			status = "down"
		}
	}
	logCxt = logCxt.WithFields(log.Fields{
		"known":   known,
		"failed":  failed,
		"operUp":  operUp,
		"adminUp": adminUp,
		"status":  status,
	})
	logCxt.Info("Re-evaluated workload endpoint status")
	return status
}

func (m *endpointManager) calculateHostEndpointStatus(id proto.HostEndpointID) string {
	logCxt := log.WithField("hostEndpointID", id)
	logCxt.Debug("Re-evaluating host endpoint status")
	var resolved, operUp bool
	_, known := m.rawHostEndpoints[id]
	if known {
		var ifaceName string
		ifaceName, resolved = m.activeHostEpIDToIfaceName[id]
		if resolved {
			operUp = m.activeUpIfaces.Contains(ifaceName)
		}
	}

	var status string
	if known {
		if operUp {
			status = "up"
		} else if resolved {
			status = "down"
		} else {
			// Known but failed to resolve, map that to error.
			status = "error"
		}
	}

	logCxt = logCxt.WithFields(log.Fields{
		"known":    known,
		"resolved": resolved,
		"operUp":   operUp,
		"status":   status,
	})
	logCxt.Info("Re-evaluated host endpoint status")
	return status
}

func (m *endpointManager) resolveWorkloadEndpoints() error {
	// Optimisation, only recalculate the dispatch chains if we've never done so or if the
	// workloads have changed in some way.
	needToCheckDispatchChains := m.activeDispatchChains == nil || len(m.pendingWlEpUpdates) > 0

	// Update any dirty endpoints.
	for id, workload := range m.pendingWlEpUpdates {
		logCxt := log.WithField("id", id)
		oldWorkload := m.activeWlEndpoints[id]
		if workload != nil {
			logCxt.Info("Updating per-endpoint chains.")
			if oldWorkload != nil && oldWorkload.Name != workload.Name {
				logCxt.Debug("Interface name changed, cleaning up old state")
				m.filterTable.RemoveChains(m.activeWlIDToChains[id])
				m.routeTable.SetRoutes(oldWorkload.Name, nil)
				m.wlIfaceNamesToReconfigure.Discard(oldWorkload.Name)
				delete(m.activeWlIfaceNameToID, oldWorkload.Name)
			}
			chains := m.ruleRenderer.WorkloadEndpointToIptablesChains(&id, workload)
			m.filterTable.UpdateChains(chains)
			m.activeWlIDToChains[id] = chains

			// Collect the IP prefixes that we want to route locally to this endpoint:
			logCxt.Info("Updating endpoint routes.")
			var (
				ipStrings  []string
				natInfos   []*proto.NatInfo
				addrSuffix string
			)
			if m.ipVersion == 4 {
				ipStrings = workload.Ipv4Nets
				natInfos = workload.Ipv4Nat
				addrSuffix = "/32"
			} else {
				ipStrings = workload.Ipv6Nets
				natInfos = workload.Ipv6Nat
				addrSuffix = "/128"
			}
			if len(natInfos) != 0 {
				old := ipStrings
				ipStrings = make([]string, len(old)+len(natInfos))
				copy(ipStrings, old)
				for ii, natInfo := range natInfos {
					ipStrings[len(old)+ii] = natInfo.ExtIp + addrSuffix
				}
			}

			var mac net.HardwareAddr
			if workload.Mac != "" {
				var err error
				mac, err = net.ParseMAC(workload.Mac)
				if err != nil {
					logCxt.WithError(err).Error(
						"Failed to parse endpoint's MAC address")
				}
			}
			routeTargets := make([]routetable.Target, len(ipStrings))
			for i, s := range ipStrings {
				routeTargets[i] = routetable.Target{
					CIDR:    ip.MustParseCIDR(s),
					DestMAC: mac,
				}
			}
			m.routeTable.SetRoutes(workload.Name, routeTargets)
			m.wlIfaceNamesToReconfigure.Add(workload.Name)
			m.activeWlEndpoints[id] = workload
			m.activeWlIfaceNameToID[workload.Name] = id
			delete(m.pendingWlEpUpdates, id)
		} else {
			logCxt.Info("Workload removed, deleting its chains.")
			m.filterTable.RemoveChains(m.activeWlIDToChains[id])
			if oldWorkload != nil {
				// Remove any routes from the routing table.  The RouteTable will
				// remove any conntrack entries as a side-effect.
				logCxt.Info("Workload removed, deleting old state.")
				m.routeTable.SetRoutes(oldWorkload.Name, nil)
				m.wlIfaceNamesToReconfigure.Discard(oldWorkload.Name)
				delete(m.activeWlIfaceNameToID, oldWorkload.Name)
			}
			delete(m.activeWlEndpoints, id)
			delete(m.pendingWlEpUpdates, id)
		}

		// Update or deletion, make sure we update the interface status.
		m.epIDsToUpdateStatus.Add(id)
	}

	if needToCheckDispatchChains {
		// Rewrite the dispatch chains if they've changed.
		newDispatchChains := m.ruleRenderer.WorkloadDispatchChains(m.activeWlEndpoints)
		if !reflect.DeepEqual(newDispatchChains, m.activeDispatchChains) {
			log.Info("Workloads changed, updating dispatch chains.")
			m.filterTable.RemoveChains(m.activeDispatchChains)
			m.filterTable.UpdateChains(newDispatchChains)
			m.activeDispatchChains = newDispatchChains
		}
	}

	m.wlIfaceNamesToReconfigure.Iter(func(item interface{}) error {
		ifaceName := item.(string)
		err := m.configureInterface(ifaceName)
		if err != nil {
			log.WithError(err).Warn("Failed to configure interface, will retry")
			return nil
		}
		return set.RemoveItem
	})

	return nil
}

func (m *endpointManager) resolveHostEndpoints() error {

	// Host endpoint resolution
	// ------------------------
	//
	// There is a set of non-workload interfaces on the local host, each possibly with
	// IP addresses, that might be controlled by HostEndpoint resources in the Calico
	// data model.  The data model syntactically allows multiple HostEndpoint
	// resources to match a given interface - for example, an interface 'eth1' might
	// have address 10.240.0.34 and 172.19.2.98, and the data model might include:
	//
	// - HostEndpoint A with Name 'eth1'
	//
	// - HostEndpoint B with ExpectedIpv4Addrs including '10.240.0.34'
	//
	// - HostEndpoint C with ExpectedIpv4Addrs including '172.19.2.98'.
	//
	// but at runtime, at any given time, we only allow one HostEndpoint to govern
	// that interface.  That HostEndpoint becomes the active one, and the others
	// remain inactive.  (But if, for example, the active HostEndpoint resource was
	// deleted, then one of the inactive ones could take over.)  Given multiple
	// matching HostEndpoint resources, the one that wins is the one with the
	// alphabetically earliest HostEndpointId
	//
	// So the process here is not about 'resolving' a particular HostEndpoint on its
	// own.  Rather it is looking at the set of local non-workload interfaces and
	// seeing which of them are matched by the current set of HostEndpoints as a
	// whole.
	m.activeIfaceNameToHostEpID = map[string]proto.HostEndpointID{}
	m.activeHostEpIDToIfaceName = map[proto.HostEndpointID]string{}
	for ifaceName, ifaceAddrs := range m.hostIfaceToAddrs {
		ifaceCxt := log.WithFields(log.Fields{
			"ifaceName":  ifaceName,
			"ifaceAddrs": ifaceAddrs,
		})
		bestHostEpId := proto.HostEndpointID{}
	HostEpLoop:
		for id, hostEp := range m.rawHostEndpoints {
			logCxt := ifaceCxt.WithField("id", id)
			logCxt.WithField("bestHostEpId", bestHostEpId).Debug("See if HostEp matches interface")
			if (bestHostEpId.EndpointId != "") && (bestHostEpId.EndpointId < id.EndpointId) {
				// We already have a HostEndpointId that is better than
				// this one, so no point looking any further.
				logCxt.Debug("No better than existing match")
				continue
			}
			if hostEp.Name == ifaceName {
				// The HostEndpoint has an explicit name that matches the
				// interface.
				logCxt.Debug("Match on explicit iface name")
				bestHostEpId = id
				continue
			} else if hostEp.Name != "" {
				// The HostEndpoint has an explicit name that isn't this
				// interface.  Continue, so as not to allow it to match on
				// an IP address instead.
				logCxt.Debug("Rejected on explicit iface name")
				continue
			}
			for _, wantedList := range [][]string{hostEp.ExpectedIpv4Addrs, hostEp.ExpectedIpv6Addrs} {
				for _, wanted := range wantedList {
					logCxt.WithField("wanted", wanted).Debug("Address wanted by HostEp")
					if ifaceAddrs.Contains(wanted) {
						// The HostEndpoint expects an IP address
						// that is on this interface.
						logCxt.Debug("Match on address")
						bestHostEpId = id
						continue HostEpLoop
					}
				}
			}
		}
		if bestHostEpId.EndpointId != "" {
			log.WithFields(log.Fields{
				"ifaceName":    ifaceName,
				"bestHostEpId": bestHostEpId,
			}).Debug("Got HostEp for interface")
			m.activeIfaceNameToHostEpID[ifaceName] = bestHostEpId
			m.activeHostEpIDToIfaceName[bestHostEpId] = ifaceName
		}
	}

	// Set up programming for the host endpoints that are now to be used.
	newHostIfaceChains := map[string][]*iptables.Chain{}
	for ifaceName, id := range m.activeIfaceNameToHostEpID {
		log.WithField("id", id).Info("Updating host endpoint chains.")
		hostEp := m.rawHostEndpoints[id]
		chains := m.ruleRenderer.HostEndpointToIptablesChains(ifaceName, hostEp)
		if !reflect.DeepEqual(chains, m.activeHostIfaceToChains[ifaceName]) {
			m.filterTable.UpdateChains(chains)
		}
		newHostIfaceChains[ifaceName] = chains
		delete(m.activeHostIfaceToChains, ifaceName)
	}

	// Remove programming for host endpoints that are not now in use.
	for ifaceName, chains := range m.activeHostIfaceToChains {
		log.WithField("ifaceName", ifaceName).Info("Host interface no longer protected, deleting its chains.")
		m.filterTable.RemoveChains(chains)
	}

	// Remember the host endpoints that are now in use.
	m.activeHostIfaceToChains = newHostIfaceChains

	// Rewrite the dispatch chains if they've changed.
	log.WithField("resolvedHostEpIds", m.activeIfaceNameToHostEpID).Debug("Rewrite dispatch chains?")
	newDispatchChains := m.ruleRenderer.HostDispatchChains(m.activeIfaceNameToHostEpID)
	if !reflect.DeepEqual(newDispatchChains, m.activeHostDispatchChains) {
		log.Info("HostEps changed, updating dispatch chains.")
		m.filterTable.RemoveChains(m.activeHostDispatchChains)
		m.filterTable.UpdateChains(newDispatchChains)
		m.activeHostDispatchChains = newDispatchChains
	}

	return nil
}

func (m *endpointManager) configureInterface(name string) error {
	if !m.activeUpIfaces.Contains(name) {
		log.WithField("ifaceName", name).Info(
			"Skipping configuration of interface because it is oper down.")
		return nil
	}
	log.WithField("ifaceName", name).Info(
		"Applying /proc/sys configuration to interface.")
	if m.ipVersion == 4 {
		err := m.writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", name), "1")
		if err != nil {
			return err
		}
		err = m.writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/route_localnet", name), "1")
		if err != nil {
			return err
		}
		err = m.writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", name), "1")
		if err != nil {
			return err
		}
		err = m.writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/neigh/%s/proxy_delay", name), "0")
		if err != nil {
			return err
		}
	} else {
		err := m.writeProcSys(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/proxy_ndp", name), "1")
		if err != nil {
			return err
		}
	}
	return nil
}

func writeProcSys(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	n, err := f.Write([]byte(value))
	if err == nil && n < len(value) {
		err = io.ErrShortWrite
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}
