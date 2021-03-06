// +build linux

/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package kvm

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"strings"
	"text/template"
	"time"

	"github.com/docker/machine/libmachine/log"
	libvirt "github.com/libvirt/libvirt-go"
	"github.com/pkg/errors"
	"k8s.io/minikube/pkg/network"
	"k8s.io/minikube/pkg/util/retry"
)

// Replace with hardcoded range with CIDR
// https://play.golang.org/p/m8TNTtygK0
const networkTmpl = `
<network>
  <name>{{.Name}}</name>
  <dns enable='no'/>
  {{with .Parameters}}
  <ip address='{{.Gateway}}' netmask='{{.Netmask}}'>
    <dhcp>
      <range start='{{.ClientMin}}' end='{{.ClientMax}}'/>
    </dhcp>
  </ip>
  {{end}}
</network>
`

type kvmNetwork struct {
	Name string
	network.Parameters
}

// firstSubnetAddr is starting subnet to try for new KVM cluster,
// avoiding possible conflict with other local networks by further incrementing it up to 20 times by 10.
const firstSubnetAddr = "192.168.39.0"

// setupNetwork ensures that the network with `name` is started (active)
// and has the autostart feature set.
func setupNetwork(conn *libvirt.Connect, name string) error {
	n, err := conn.LookupNetworkByName(name)
	if err != nil {
		return errors.Wrapf(err, "checking network %s", name)
	}
	defer func() { _ = n.Free() }()

	// always ensure autostart is set on the network
	autostart, err := n.GetAutostart()
	if err != nil {
		return errors.Wrapf(err, "checking network %s autostart", name)
	}
	if !autostart {
		if err := n.SetAutostart(true); err != nil {
			return errors.Wrapf(err, "setting autostart for network %s", name)
		}
	}

	// always ensure the network is started (active)
	active, err := n.IsActive()
	if err != nil {
		return errors.Wrapf(err, "checking network status for %s", name)
	}
	if !active {
		if err := n.Create(); err != nil {
			return errors.Wrapf(err, "starting network %s", name)
		}
	}
	return nil
}

// ensureNetwork is called on start of the VM
func (d *Driver) ensureNetwork() error {
	conn, err := getConnection(d.ConnectionURI)
	if err != nil {
		return errors.Wrap(err, "getting libvirt connection")
	}
	defer conn.Close()

	// network: default

	// It is assumed that the libvirt/kvm installation has already created this network
	log.Infof("Ensuring network %s is active", d.Network)
	if err := setupNetwork(conn, d.Network); err != nil {
		return err
	}

	// network: private

	// Start the private network
	log.Infof("Ensuring network %s is active", d.PrivateNetwork)
	// retry once to recreate the network, but only if is not used by another minikube instance
	if err := setupNetwork(conn, d.PrivateNetwork); err != nil {
		log.Debugf("Network %s is inoperable, will try to recreate it: %v", d.PrivateNetwork, err)
		if err := d.deleteNetwork(); err != nil {
			return errors.Wrapf(err, "deleting inoperable network %s", d.PrivateNetwork)
		}
		log.Debugf("Successfully deleted %s network", d.PrivateNetwork)
		if err := d.createNetwork(); err != nil {
			return errors.Wrapf(err, "recreating inoperable network %s", d.PrivateNetwork)
		}
		log.Debugf("Successfully recreated %s network", d.PrivateNetwork)
		if err := setupNetwork(conn, d.PrivateNetwork); err != nil {
			return err
		}
		log.Debugf("Successfully activated %s network", d.PrivateNetwork)
	}

	return nil
}

// createNetwork is called during creation of the VM only (and not on start)
func (d *Driver) createNetwork() error {
	if d.Network == defaultPrivateNetworkName {
		return fmt.Errorf("KVM network can't be named %s. This is the name of the private network created by minikube", defaultPrivateNetworkName)
	}

	conn, err := getConnection(d.ConnectionURI)
	if err != nil {
		return errors.Wrap(err, "getting libvirt connection")
	}
	defer conn.Close()

	// network: default
	// It is assumed that the libvirt/kvm installation has already created this network
	netd, err := conn.LookupNetworkByName(d.Network)
	if err != nil {
		return errors.Wrapf(err, "%s KVM network doesn't exist", d.Network)
	}
	log.Debugf("found existing %s KVM network", d.Network)
	if netd != nil {
		_ = netd.Free()
	}

	// network: private
	// Only create the private network if it does not already exist
	netp, err := conn.LookupNetworkByName(d.PrivateNetwork)
	defer func() {
		if netp != nil {
			_ = netp.Free()
		}
	}()
	if err == nil {
		log.Debugf("found existing private KVM network %s", d.PrivateNetwork)
		return nil
	}

	// retry up to 5 times to create kvm network
	for attempts, subnetAddr := 0, firstSubnetAddr; attempts < 5; attempts++ {
		// Rather than iterate through all of the valid subnets, give up at 20 to avoid a lengthy user delay for something that is unlikely to work.
		// will be like 192.168.39.0/24,..., 192.168.248.0/24 (in increment steps of 11)
		var subnet *network.Parameters
		subnet, err = network.FreeSubnet(subnetAddr, 11, 20)
		if err != nil {
			log.Debugf("failed to find free subnet for private KVM network %s after %d attempts: %v", d.PrivateNetwork, 20, err)
			return fmt.Errorf("un-retryable: %w", err)
		}
		// create the XML for the private network from our networkTmpl
		tryNet := kvmNetwork{
			Name:       d.PrivateNetwork,
			Parameters: *subnet,
		}
		tmpl := template.Must(template.New("network").Parse(networkTmpl))
		var networkXML bytes.Buffer
		if err = tmpl.Execute(&networkXML, tryNet); err != nil {
			return fmt.Errorf("executing private KVM network template: %w", err)
		}
		// define the network using our template
		var network *libvirt.Network
		network, err = conn.NetworkDefineXML(networkXML.String())
		if err != nil {
			return fmt.Errorf("defining private KVM network %s %s from xml %s: %w", d.PrivateNetwork, subnet.CIDR, networkXML.String(), err)
		}
		// and finally create & start it
		log.Debugf("trying to create private KVM network %s %s...", d.PrivateNetwork, subnet.CIDR)
		if err = network.Create(); err == nil {
			log.Debugf("private KVM network %s %s created", d.PrivateNetwork, subnet.CIDR)
			return nil
		}
		log.Debugf("failed to create private KVM network %s %s, will retry: %v", d.PrivateNetwork, subnet.CIDR, err)
		subnetAddr = subnet.IP
	}
	return fmt.Errorf("failed to create private KVM network %s: %w", d.PrivateNetwork, err)
}

func (d *Driver) deleteNetwork() error {
	conn, err := getConnection(d.ConnectionURI)
	if err != nil {
		return errors.Wrap(err, "getting libvirt connection")
	}
	defer conn.Close()

	// network: default
	// It is assumed that the OS manages this network

	// network: private
	log.Debugf("Checking if network %s exists...", d.PrivateNetwork)
	network, err := conn.LookupNetworkByName(d.PrivateNetwork)
	if err != nil {
		if lvErr(err).Code == libvirt.ERR_NO_NETWORK {
			log.Warnf("Network %s does not exist. Skipping deletion", d.PrivateNetwork)
			return nil
		}
		return errors.Wrapf(err, "failed looking up network %s", d.PrivateNetwork)
	}
	defer func() { _ = network.Free() }()
	log.Debugf("Network %s exists", d.PrivateNetwork)

	err = d.checkDomains(conn)
	if err != nil {
		return err
	}

	// when we reach this point, it means it is safe to delete the network

	log.Debugf("Trying to delete network %s...", d.PrivateNetwork)
	delete := func() error {
		active, err := network.IsActive()
		if err != nil {
			return err
		}
		if active {
			log.Debugf("Destroying active network %s", d.PrivateNetwork)
			if err := network.Destroy(); err != nil {
				return err
			}
		}
		log.Debugf("Undefining inactive network %s", d.PrivateNetwork)
		return network.Undefine()
	}
	if err := retry.Local(delete, 10*time.Second); err != nil {
		return errors.Wrap(err, "deleting network")
	}
	log.Debugf("Network %s deleted", d.PrivateNetwork)

	return nil
}

func (d *Driver) checkDomains(conn *libvirt.Connect) error {
	type source struct {
		// XMLName xml.Name `xml:"source"`
		Network string `xml:"network,attr"`
	}
	type iface struct {
		// XMLName xml.Name `xml:"interface"`
		Source source `xml:"source"`
	}
	type result struct {
		// XMLName xml.Name `xml:"domain"`
		Name       string  `xml:"name"`
		Interfaces []iface `xml:"devices>interface"`
	}

	// iterate over every (also turned off) domains, and check if it
	// is using the private network. Do *not* delete the network if
	// that is the case
	log.Debug("Trying to list all domains...")
	doms, err := conn.ListAllDomains(0)
	if err != nil {
		return errors.Wrap(err, "list all domains")
	}
	log.Debugf("Listed all domains: total of %d domains", len(doms))

	// fail if there are 0 domains
	if len(doms) == 0 {
		log.Warn("list of domains is 0 length")
	}

	for _, dom := range doms {
		// get the name of the domain we iterate over
		log.Debug("Trying to get name of domain...")
		name, err := dom.GetName()
		if err != nil {
			return errors.Wrap(err, "failed to get name of a domain")
		}
		log.Debugf("Got domain name: %s", name)

		// skip the domain if it is our own machine
		if name == d.MachineName {
			log.Debug("Skipping domain as it is us...")
			continue
		}

		// unfortunately, there is no better way to retrieve a list of all defined interfaces
		// in domains than getting it from the defined XML of all domains
		// NOTE: conn.ListAllInterfaces does not help in this case
		log.Debugf("Getting XML for domain %s...", name)
		xmlString, err := dom.GetXMLDesc(libvirt.DOMAIN_XML_INACTIVE)
		if err != nil {
			return errors.Wrapf(err, "failed to get XML of domain '%s'", name)
		}
		log.Debugf("Got XML for domain %s", name)

		v := result{}
		err = xml.Unmarshal([]byte(xmlString), &v)
		if err != nil {
			return errors.Wrapf(err, "failed to unmarshal XML of domain '%s", name)
		}
		log.Debugf("Unmarshaled XML for domain %s: %#v", name, v)

		// iterate over the found interfaces
		for _, i := range v.Interfaces {
			if i.Source.Network == d.PrivateNetwork {
				log.Debugf("domain %s DOES use network %s, aborting...", name, d.PrivateNetwork)
				return fmt.Errorf("network still in use at least by domain '%s',", name)
			}
			log.Debugf("domain %s does not use network %s", name, d.PrivateNetwork)
		}
	}

	return nil
}

func (d *Driver) lookupIP() (string, error) {
	conn, err := getConnection(d.ConnectionURI)
	if err != nil {
		return "", errors.Wrap(err, "getting connection and domain")
	}
	defer conn.Close()

	libVersion, err := conn.GetLibVersion()
	if err != nil {
		return "", errors.Wrap(err, "getting libversion")
	}

	// Earlier versions of libvirt use a lease file instead of a status file
	if libVersion < 1002006 {
		return d.lookupIPFromLeasesFile()
	}

	// TODO: for everything > 1002006, there is direct support in the libvirt-go for handling this
	return d.lookupIPFromStatusFile(conn)
}

func (d *Driver) lookupIPFromStatusFile(conn *libvirt.Connect) (string, error) {
	network, err := conn.LookupNetworkByName(d.PrivateNetwork)
	if err != nil {
		return "", errors.Wrap(err, "looking up network by name")
	}
	defer func() { _ = network.Free() }()

	bridge, err := network.GetBridgeName()
	if err != nil {
		log.Warnf("Failed to get network bridge: %v", err)
		return "", err
	}
	statusFile := fmt.Sprintf("/var/lib/libvirt/dnsmasq/%s.status", bridge)
	statuses, err := ioutil.ReadFile(statusFile)
	if err != nil {
		return "", errors.Wrap(err, "reading status file")
	}

	return parseStatusAndReturnIP(d.PrivateMAC, statuses)
}

func parseStatusAndReturnIP(privateMAC string, statuses []byte) (string, error) {
	type StatusEntry struct {
		IPAddress  string `json:"ip-address"`
		MacAddress string `json:"mac-address"`
	}
	var statusEntries []StatusEntry

	// empty file return blank
	if len(statuses) == 0 {
		return "", nil
	}

	err := json.Unmarshal(statuses, &statusEntries)
	if err != nil {
		return "", errors.Wrap(err, "reading status file")
	}

	for _, status := range statusEntries {
		if status.MacAddress == privateMAC {
			return status.IPAddress, nil
		}
	}

	return "", nil
}

func (d *Driver) lookupIPFromLeasesFile() (string, error) {
	leasesFile := fmt.Sprintf("/var/lib/libvirt/dnsmasq/%s.leases", d.PrivateNetwork)
	leases, err := ioutil.ReadFile(leasesFile)
	if err != nil {
		return "", errors.Wrap(err, "reading leases file")
	}
	ipAddress := ""
	for _, lease := range strings.Split(string(leases), "\n") {
		if len(lease) == 0 {
			continue
		}
		// format for lease entry
		// ExpiryTime MAC IP Hostname ExtendedMAC
		entry := strings.Split(lease, " ")
		if len(entry) != 5 {
			return "", fmt.Errorf("malformed leases entry: %s", entry)
		}
		if entry[1] == d.PrivateMAC {
			ipAddress = entry[2]
		}
	}
	return ipAddress, nil
}
