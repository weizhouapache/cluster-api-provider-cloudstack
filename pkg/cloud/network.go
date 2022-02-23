/*
Copyright 2022 The Kubernetes Authors.

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

package cloud

import (
	"strconv"
	"strings"

	"github.com/apache/cloudstack-go/v2/cloudstack"
	infrav1 "github.com/aws/cluster-api-provider-cloudstack/api/v1beta1"
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
)

type NetworkIface interface {
	ResolveNetworkStatuses(*infrav1.CloudStackCluster) error
	CreateIsolatedNewtork(*infrav1.CloudStackCluster) error
	OpenFirewallRules(*infrav1.CloudStackCluster) error
	ResolvePublicIPDetails(*infrav1.CloudStackCluster) (*cloudstack.PublicIpAddress, error)
	ResolveLoadBalancerRuleDetails(*infrav1.CloudStackCluster) error
	GetOrCreateLoadBalancerRule(*infrav1.CloudStackCluster) error
}

const (
	NetOffering          = "DefaultIsolatedNetworkOfferingWithSourceNatService"
	K8sDefaultAPIPort    = 6443
	NetworkTypeIsolated  = "Isolated"
	NetworkTypeShared    = "Shared"
	NetworkProtocolTCP   = "tcp"
	addCreatedByTag      = true
	doNotAddCreatedByTag = false
)

// usesIsolatedNetwork returns true if this cluster is specs an isolated network.
// Assumes that the a fetch has been done on network statuses prior.
func usesIsolatedNetwork(csCluster *infrav1.CloudStackCluster) bool {
	firstNetStatus := csCluster.Status.Zones[csCluster.Spec.Zones[0].Network.Name].Network
	// Check for Isolated network use case.
	if len(csCluster.Spec.Zones) == 1 { // Where the only specced network
		if firstNetStatus.Type == "" || // doesn't exist or
			firstNetStatus.Type == NetworkTypeIsolated { // exists and is an isolated network.
			return true
		}
	}
	return false
}

// networkExists checks that the network already exists based on the presence of all fields.
// Assumes that the a fetch has been done on network statuses prior.
func networkExists(net infrav1.Network) bool {
	if net.Name != "" && net.Type != "" && net.Id != "" {
		return true
	}
	return false
}

// ResolveNetworks fetches networks' Id, Name, and Type.
func (c *client) ResolveNetwork(csCluster *infrav1.CloudStackCluster, net *infrav1.Network) (retErr error) {
	netName := net.Name
	netId, count, err := c.cs.Network.GetNetworkID(netName)
	if err != nil {
		retErr = multierror.Append(retErr, errors.Wrapf(err, "Could not get Network ID from %s.", netName))
	} else if count != 1 {
		retErr = multierror.Append(retErr, errors.Errorf(
			"Expected 1 Network with name %s, but got %d.", netName, count))
	} else { // Got netId from the network's name.
		netId = net.Id
	}

	// Now get network details.
	netDetails, count, err := c.cs.Network.GetNetworkByID(netId)
	if err != nil {
		return multierror.Append(retErr, errors.Wrapf(
			err, "Could not get Network by ID %s.", netId))
	} else if count != 1 {
		return multierror.Append(retErr, errors.Errorf(
			"Expected 1 Network with UUID %s, but got %d.", netId, count))
	}
	net.Name = netDetails.Name
	net.Id = netDetails.Id
	net.Type = netDetails.Type
	return nil
}

func generateNetworkTagName(csCluster *infrav1.CloudStackCluster) string {
	return clusterTagNamePrefix + string(csCluster.UID)
}

// getOfferingId fetches an offering id.
func (c *client) getOfferingId() (string, error) {
	offeringId, count, retErr := c.cs.NetworkOffering.GetNetworkOfferingID(NetOffering)
	if retErr != nil {
		return "", retErr
	} else if count != 1 {
		return "", errors.New("found more than one network offering.")
	}
	return offeringId, nil
}

// CreateIsolatedNewtork creates an isolated network in the relevant Zone.
// Assumes that there is only the one zone in the cluster.
func (c *client) CreateIsolatedNewtork(csCluster *infrav1.CloudStackCluster) (retErr error) {
	zoneStatus := csCluster.Status.Zones[csCluster.Spec.Zones[0].Network.Name]
	netStatus := zoneStatus.Network

	// Fetch offering Id.
	offeringId, err := c.getOfferingId()
	if err != nil {
		return err
	}

	// Do creation.
	p := c.cs.Network.NewCreateNetworkParams(netStatus.Name, netStatus.Name, offeringId, zoneStatus.Id)
	setIfNotEmpty(csCluster.Spec.Account, p.SetAccount)
	setIfNotEmpty(csCluster.Status.DomainID, p.SetDomainid)
	resp, err := c.cs.Network.CreateNetwork(p)
	if err != nil {
		return err
	}
	c.addClusterTags(csCluster, zoneStatus.Network, addCreatedByTag)

	// Update Zone/Network status accordingly.
	netStatus.Id = resp.Id
	netStatus.Type = resp.Type
	zoneStatus.Network = netStatus
	csCluster.Status.Zones[zoneStatus.Name] = zoneStatus

	return nil
}

// ResolveNetworkStatuses fetches details on all networks specced, but will not modify ACS settings.
func (c *client) ResolveNetworkStatuses(csCluster *infrav1.CloudStackCluster) (retErr error) {
	// Copy network spec to status in preparation for network resolution or creation.
	for _, specZone := range csCluster.Spec.Zones {
		zone, ok := csCluster.Status.Zones[specZone.Name]
		if !ok {
			csCluster.Status.Zones[specZone.Name] = specZone
			zone = specZone
		}
		zone.Network = specZone.Network
	}

	// At this point network status should have been populated (copied) from the spec.
	for _, zoneStatus := range csCluster.Status.Zones {
		if retErr = c.ResolveNetwork(csCluster, &zoneStatus.Network); retErr == nil { // Found network
			c.addClusterTags(csCluster, zoneStatus.Network, doNotAddCreatedByTag)
			continue
		} else if !strings.Contains(retErr.Error(), "No match found") { // Some other error.
			return retErr
		} // Network not found, so create it.
	}

	return nil
}

func (c *client) addClusterTags(csCluster *infrav1.CloudStackCluster, net infrav1.Network, addCreatedBy bool) error {
	clusterTagName := generateNetworkTagName(csCluster)
	newTags := map[string]string{}

	existingTags, err := c.GetNetworkTags(net.Id)
	if err != nil {
		return err
	}

	if existingTags[clusterTagName] == "" {
		newTags[clusterTagName] = "1"
	}

	if addCreatedBy && existingTags[createdByCapcTagName] == "" {
		newTags[createdByCapcTagName] = "1"
	}

	if len(newTags) > 0 {
		return c.AddNetworkTags(net.Id, newTags)
	}

	return nil
}

func (c *client) RemoveClusterTagFromNetwork(csCluster *infrav1.CloudStackCluster, net infrav1.Network) (retError error) {
	tags, err := c.GetNetworkTags(net.Id)
	if err != nil {
		return err
	}
	sourceNAT := publicIP != nil && publicIP.Issourcenat

	clusterTagName := generateNetworkTagName(csCluster)
	if tagValue := tags[clusterTagName]; tagValue != "" {
		if err = c.DeleteNetworkTags(net.Id, map[string]string{clusterTagName: tagValue}); err != nil {
			return err
		}
	}

	return nil
}

func (c *client) DeleteNetworkIfNotInUse(csCluster *infrav1.CloudStackCluster, net infrav1.Network) (retError error) {
	tags, err := c.GetNetworkTags(net.Id)
	if err != nil {
		return err
	}

	var clusterTagCount int
	for tagName := range tags {
		if strings.HasPrefix(tagName, clusterTagNamePrefix) {
			clusterTagCount++
		}
	}

	if clusterTagCount == 0 && tags[createdByCapcTagName] != "" {
		return c.DestroyNetwork(net)
	}

	return nil
}

func (c *client) ResolvePublicIPDetails(csCluster *infrav1.CloudStackCluster) (*cloudstack.PublicIpAddress, error) {
	ip := csCluster.Spec.ControlPlaneEndpoint.Host

	p := c.cs.Address.NewListPublicIpAddressesParams()
	p.SetAllocatedonly(false)
	setIfNotEmpty(csCluster.Spec.Account, p.SetAccount)
	setIfNotEmpty(csCluster.Status.DomainID, p.SetDomainid)
	if ip != "" {
		p.SetIpaddress(ip)
	}
	publicAddresses, err := c.cs.Address.ListPublicIpAddresses(p)

	if err != nil {
		return nil, err
	} else if ip != "" && publicAddresses.Count == 1 { // Endpoint specified and IP found.
		// Ignore already allocated here since the IP was specified.
		return publicAddresses.PublicIpAddresses[0], nil
	} else if publicAddresses.Count > 0 { // Endpoint not specified.
		for _, v := range publicAddresses.PublicIpAddresses { // Pick first available address.
			if v.Allocated == "" { // Found un-allocated Public IP.
				return v, nil
			}
		}
		return nil, errors.New("All Public IP Adresse(s) found were already allocated.")
	}
	return nil, errors.New("No public addresses found in available networks.")
}

// AssociatePublicIPAddress Gets a PublicIP and associates it.
func (c *client) AssociatePublicIPAddress(csCluster *infrav1.CloudStackCluster) (retErr error) {
	publicAddress, err := c.ResolvePublicIPDetails(csCluster)
	if err != nil {
		return err
	}

	csCluster.Spec.ControlPlaneEndpoint.Host = publicAddress.Ipaddress
	csCluster.Status.PublicIPID = publicAddress.Id

	if publicAddress.Allocated != "" {
		// Address already allocated to network. Allocated is a timestamp -- not a boolean.
		return c.AddClusterTag(ResourceTypeIPAddress, publicAddress.Id, csCluster, false)
	} // Address not yet allocated. Allocate now.

	// Public IP found, but not yet allocated to network.
	p := c.cs.Address.NewAssociateIpAddressParams()
	p.SetIpaddress(csCluster.Spec.ControlPlaneEndpoint.Host)
	setIfNotEmpty(csCluster.Spec.Account, p.SetAccount)
	setIfNotEmpty(csCluster.Status.DomainID, p.SetDomainid)
	resp, err := c.cs.Address.AssociateIpAddress(p)
	if err != nil {
		return err
	}
	csCluster.Status.PublicIPNetworkId = resp.Networkid
	return nil
}

func (c *client) OpenFirewallRules(csCluster *infrav1.CloudStackCluster) (retErr error) {
	p := c.cs.Firewall.NewCreateEgressFirewallRuleParams(csCluster.Status.PublicIPNetworkId, NetworkProtocolTCP)
	_, retErr = c.cs.Firewall.CreateEgressFirewallRule(p)
	if retErr != nil && strings.Contains(strings.ToLower(retErr.Error()), "there is already") { // Already a firewall rule here.
		retErr = nil
	}
	return retErr
}

func (c *client) ResolveLoadBalancerRuleDetails(csCluster *infrav1.CloudStackCluster) (retErr error) {
	p := c.cs.LoadBalancer.NewListLoadBalancerRulesParams()
	p.SetPublicipid(csCluster.Status.PublicIPID)
	setIfNotEmpty(csCluster.Spec.Account, p.SetAccount)
	setIfNotEmpty(csCluster.Status.DomainID, p.SetDomainid)
	loadBalancerRules, err := c.cs.LoadBalancer.ListLoadBalancerRules(p)
	if err != nil {
		return err
	}
	for _, rule := range loadBalancerRules.LoadBalancerRules {
		if rule.Publicport == strconv.Itoa(int(csCluster.Spec.ControlPlaneEndpoint.Port)) {
			csCluster.Status.LBRuleID = rule.Id
			return nil
		}
	}
	return errors.New("no load balancer rule found")
}

// GetOrCreateLoadBalancerRule Create a load balancer rule that can be assigned to instances.
func (c *client) GetOrCreateLoadBalancerRule(csCluster *infrav1.CloudStackCluster) (retErr error) {
	// Check if rule exists.
	if err := c.ResolveLoadBalancerRuleDetails(csCluster); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "no load balancer rule found") {
		return err
	}

	p := c.cs.LoadBalancer.NewCreateLoadBalancerRuleParams(
		"roundrobin", "Kubernetes_API_Server", K8sDefaultAPIPort, K8sDefaultAPIPort)
	p.SetNetworkid(csCluster.Status.PublicIPNetworkId)
	if csCluster.Spec.ControlPlaneEndpoint.Port != 0 { // Override default public port if endpoint port specified.
		p.SetPublicport(int(csCluster.Spec.ControlPlaneEndpoint.Port))
	}
	p.SetPublicipid(csCluster.Status.PublicIPID)
	p.SetProtocol(NetworkProtocolTCP)
	setIfNotEmpty(csCluster.Spec.Account, p.SetAccount)
	setIfNotEmpty(csCluster.Status.DomainID, p.SetDomainid)
	resp, err := c.cs.LoadBalancer.CreateLoadBalancerRule(p)
	if err != nil {
		return err
	}
	csCluster.Status.LBRuleID = resp.Id
	return nil
}

func (c *client) DestroyNetwork(net infrav1.Network) (retErr error) {
	_, retErr = c.cs.Network.DeleteNetwork(c.cs.Network.NewDeleteNetworkParams(net.Id))
	return retErr
}

func (c *client) AssignVMToLoadBalancerRule(csCluster *infrav1.CloudStackCluster, instanceID string) (retErr error) {

	// Check that the instance isn't already in LB rotation.
	lbRuleInstances, retErr := c.cs.LoadBalancer.ListLoadBalancerRuleInstances(
		c.cs.LoadBalancer.NewListLoadBalancerRuleInstancesParams(csCluster.Status.LBRuleID))
	if retErr != nil {
		return retErr
	}
	for _, instance := range lbRuleInstances.LoadBalancerRuleInstances {
		if instance.Id == instanceID { // Already assigned to load balancer..
			return nil
		}
	}

	// Assign to Load Balancer.
	p := c.cs.LoadBalancer.NewAssignToLoadBalancerRuleParams(csCluster.Status.LBRuleID)
	p.SetVirtualmachineids([]string{instanceID})
	_, retErr = c.cs.LoadBalancer.AssignToLoadBalancerRule(p)
	return retErr
}
