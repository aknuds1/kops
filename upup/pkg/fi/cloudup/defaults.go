/*
Copyright 2016 The Kubernetes Authors.

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

package cloudup

import (
	"fmt"
	"net"
	"strings"

	"github.com/golang/glog"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/util/pkg/vfs"

	kopsversion "k8s.io/kops"
)

// PerformAssignments populates values that are required and immutable
// For example, it assigns stable Keys to InstanceGroups & Masters, and
// it assigns CIDRs to subnets
// We also assign KubernetesVersion, because we want it to be explicit
//
// PerformAssignments is called on create, as well as an update. In fact
// any time Run() is called in apply_cluster.go we will reach this function.
// Please do all after-market logic here.
//
func PerformAssignments(c *kops.Cluster) error {
	cloud, err := BuildCloud(c)
	if err != nil {
		return err
	}

	if c.SharedVPC() && c.Spec.NetworkCIDR == "" {
		vpcInfo, err := cloud.FindVPCInfo(c.Spec.NetworkID)
		if err != nil {
			return err
		}
		if vpcInfo == nil {
			return fmt.Errorf("unable to find VPC ID %q", c.Spec.NetworkID)
		}
		c.Spec.NetworkCIDR = vpcInfo.CIDR
		if c.Spec.NetworkCIDR == "" {
			return fmt.Errorf("Unable to infer NetworkCIDR from VPC ID, please specify --network-cidr")
		}
	}

	// Topology support
	// TODO Kris: Unsure if this needs to be here, or if the API conversion code will handle it
	if c.Spec.Topology == nil {
		c.Spec.Topology = &kops.TopologySpec{Masters: kops.TopologyPublic, Nodes: kops.TopologyPublic}
	}

	if c.Spec.NetworkCIDR == "" && !c.SharedVPC() {
		// TODO: Choose non-overlapping networking CIDRs for VPCs, using vpcInfo
		c.Spec.NetworkCIDR = "172.20.0.0/16"
	}

	if c.Spec.NonMasqueradeCIDR == "" {
		c.Spec.NonMasqueradeCIDR = "100.64.0.0/10"
	}

	// TODO: Unclear this should be here - it isn't too hard to change
	if c.Spec.MasterPublicName == "" && c.ObjectMeta.Name != "" {
		c.Spec.MasterPublicName = "api." + c.ObjectMeta.Name
	}

	// TODO: Use vpcInfo
	err = assignCIDRsToSubnets(c)
	if err != nil {
		return err
	}

	c.Spec.EgressProxy, err = assignProxy(c)
	if err != nil {
		return err
	}

	return ensureKubernetesVersion(c)
}

// ensureKubernetesVersion populates KubernetesVersion, if it is not already set
// It will be populated with the latest stable kubernetes version, or the version from the channel
func ensureKubernetesVersion(c *kops.Cluster) error {
	if c.Spec.KubernetesVersion == "" {
		if c.Spec.Channel != "" {
			channel, err := kops.LoadChannel(c.Spec.Channel)
			if err != nil {
				return err
			}
			kubernetesVersion := kops.RecommendedKubernetesVersion(channel, kopsversion.Version)
			if kubernetesVersion != nil {
				c.Spec.KubernetesVersion = kubernetesVersion.String()
				glog.Infof("Using KubernetesVersion %q from channel %q", c.Spec.KubernetesVersion, c.Spec.Channel)
			} else {
				glog.Warningf("Cannot determine recommended kubernetes version from channel %q", c.Spec.Channel)
			}
		} else {
			glog.Warningf("Channel is not set; cannot determine KubernetesVersion from channel")
		}
	}

	if c.Spec.KubernetesVersion == "" {
		latestVersion, err := FindLatestKubernetesVersion()
		if err != nil {
			return err
		}
		glog.Infof("Using kubernetes latest stable version: %s", latestVersion)
		c.Spec.KubernetesVersion = latestVersion
	}
	return nil
}

// FindLatestKubernetesVersion returns the latest kubernetes version,
// as stored at https://storage.googleapis.com/kubernetes-release/release/stable.txt
// This shouldn't be used any more; we prefer reading the stable channel
func FindLatestKubernetesVersion() (string, error) {
	stableURL := "https://storage.googleapis.com/kubernetes-release/release/stable.txt"
	glog.Warningf("Loading latest kubernetes version from %q", stableURL)
	b, err := vfs.Context.ReadFile(stableURL)
	if err != nil {
		return "", fmt.Errorf("KubernetesVersion not specified, and unable to download latest version from %q: %v", stableURL, err)
	}
	latestVersion := strings.TrimSpace(string(b))
	return latestVersion, nil
}

func assignProxy(cluster *kops.Cluster) (*kops.EgressProxySpec, error) {

	egressProxy := cluster.Spec.EgressProxy
	// Add default no_proxy values if we are using a http proxy
	if egressProxy != nil {

		var egressSlice []string
		if egressProxy.ProxyExcludes != "" {
			egressSlice = strings.Split(egressProxy.ProxyExcludes, ",")
		}

		ip, _, err := net.ParseCIDR(cluster.Spec.NonMasqueradeCIDR)
		if err != nil {
			return nil, fmt.Errorf("unable to parse Non Masquerade CIDR")
		}

		firstIP, err := incrementIP(ip, cluster.Spec.NonMasqueradeCIDR)
		if err != nil {
			return nil, fmt.Errorf("unable to get first ip address in Non Masquerade CIDR")
		}

		// run through the basic list
		for _, exclude := range []string{
			"127.0.0.1",
			"localhost",
			cluster.Spec.ClusterDNSDomain, // TODO we may want this for public loadbalancers
			cluster.Spec.MasterPublicName,
			cluster.ObjectMeta.Name,
			firstIP,
			cluster.Spec.NonMasqueradeCIDR,
		} {
			if exclude == "" {
				continue
			}
			if !strings.Contains(egressProxy.ProxyExcludes, exclude) {
				egressSlice = append(egressSlice, exclude)
			}
		}

		awsNoProxy := "169.254.169.254"

		if cluster.Spec.CloudProvider == "aws" && !strings.Contains(cluster.Spec.EgressProxy.ProxyExcludes, awsNoProxy) {
			egressSlice = append(egressSlice, awsNoProxy)
		}

		// the kube-apiserver will need to talk to kubelets on their node IP addresses port 10250
		// for pod logs to be available via the api
		if cluster.Spec.NetworkCIDR != "" {
			if !strings.Contains(cluster.Spec.EgressProxy.ProxyExcludes, cluster.Spec.NetworkCIDR) {
				egressSlice = append(egressSlice, cluster.Spec.NetworkCIDR)
			}
		} else {
			glog.Warningf("No NetworkCIDR defined (yet), not adding to egressProxy.excludes")
		}

		egressProxy.ProxyExcludes = strings.Join(egressSlice, ",")
		glog.V(8).Infof("Completed setting up Proxy excludes as follows: %q", egressProxy.ProxyExcludes)
	} else {
		glog.V(8).Info("Not setting up Proxy Excludes")
	}

	return egressProxy, nil
}

func incrementIP(ip net.IP, cidr string) (string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
	if !ipNet.Contains(ip) {
		return "", fmt.Errorf("overflowed CIDR while incrementing IP")
	}
	return ip.String(), nil
}
