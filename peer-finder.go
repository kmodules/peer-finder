/*
Copyright 2014 The Kubernetes Authors.

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

// A small utility program to lookup hostnames of endpoints in a service.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/kballard/go-shellquote"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
)

const (
	pollPeriod = 1 * time.Second
)

const (
	defaultTimeout = 10 * time.Second
)

type AddressType string

const (
	AddressTypeDNS AddressType = "DNS"
	// Uses spec.podIP as address for db pods.
	AddressTypeIP AddressType = "IP"
	// Uses first IPv4 address from spec.podIP, spec.podIPs fields as address for db pods.
	AddressTypeIPv4 AddressType = "IPv4"
	// Uses first IPv6 address from spec.podIP, spec.podIPs fields as address for db pods.
	AddressTypeIPv6 AddressType = "IPv6"
)

var (
	kc         kubernetes.Interface
	controller *Controller
	log        = klogr.New().WithName("peer-finder") // nolint:staticcheck
)

var (
	masterURL      = flag.String("master", "", "The address of the Kubernetes API server (overrides any value in kubeconfig)")
	kubeconfigPath = flag.String("kubeconfig", "", "Path to kubeconfig file with authorization information (the master location is set by the master flag).")
	hostsFilePath  = flag.String("hosts-file", "/etc/hosts", "Path to hosts file.")
	onChange       = flag.String("on-change", "", "Script to run on change, must accept a new line separated list of peers via stdin.")
	onStart        = flag.String("on-start", "", "Script to run on start, must accept a new line separated list of peers via stdin.")
	addrType       = flag.String("address-type", string(AddressTypeDNS), "Address type used to communicate with peers. Possible values: DNS, IP, IPv4, IPv6.")
	svc            = flag.String("service", "", "Governing service responsible for the DNS records of the domain this pod is in.")
	namespace      = flag.String("ns", "", "The namespace this pod is running in. If unspecified, the POD_NAMESPACE env var is used.")
	domain         = flag.String("domain", "", "The Cluster Domain which is used by the Cluster, if not set tries to determine it from /etc/resolv.conf file.")
	selector       = flag.String("selector", "", "The selector is used to select the pods whose ip will use to form peers")
)

func lookupDNS(svcName string) (sets.Set[string], error) {
	endpoints := sets.New[string]()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	_, srvRecords, err := net.DefaultResolver.LookupSRV(ctx, "", "", svcName)
	if err != nil {
		return endpoints, fmt.Errorf("DNS lookup failed for service %s: %w", svcName, err)
	}

	for _, srvRecord := range srvRecords {
		// The SRV records ends in a "." for the root domain
		// Trim the trailing dot
		ep := strings.TrimSuffix(srvRecord.Target, ".")
		endpoints.Insert(ep)
	}

	if endpoints.Len() == 0 {
		return endpoints, fmt.Errorf("no endpoints found for service %s", svcName)
	}

	return endpoints, nil
}

func lookupHostIPs(hostName string) (sets.Set[string], error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	ips := sets.New[string]()
	hostIPs, err := net.DefaultResolver.LookupIP(ctx, "ip", hostName)
	if err != nil {
		return nil, fmt.Errorf("IP lookup failed for host %s: %w", hostName, err)
	}

	for _, hostIP := range hostIPs {
		ips.Insert(hostIP.String())
	}

	if ips.Len() == 0 {
		return nil, fmt.Errorf("no valid IP addresses found for host %s", hostName)
	}

	return ips, nil
}

func shellOut(script string, peers, hostIPs sets.Set[string], fqHostname string) error {
	// add extra newline at the end to ensure end of line for bash read command
	sendStdin := strings.Join(sets.List(peers), "\n") + "\n"

	fields, err := shellquote.Split(script)
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		return fmt.Errorf("missing command: %s", script)
	}

	log.Info("exec", "command", fields[0], "stdin", sendStdin)
	cmd := exec.Command(fields[0], fields[1:]...)
	cmd.Stdin = strings.NewReader(sendStdin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	info, err := retrieveHostInfo(fqHostname, hostIPs, peers)
	if err != nil {
		return err
	}

	envs := sets.NewString(os.Environ()...)

	envs.Insert("HOST_ADDRESS=" + info.HostAddr)                  // fqdn, ipv4, ipv6
	envs.Insert("HOST_ADDRESS_TYPE=" + string(info.HostAddrType)) // DNS, IPv4, IPv6
	// WARNING: Potentially overwrites the POD_IP from container env before passing to script in case of IPv4 or IPv6 in a dual stack cluster
	envs.Insert("POD_IP=" + info.PodIP)                  // used for whitelist
	envs.Insert("POD_IP_TYPE=" + string(info.PodIPType)) // IPv4, IPv6

	cmd.Env = envs.List()

	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("execution failed of script=%s. reason:%v", script, err)
	}
	return nil
}

type HostInfo struct {
	// fqdn, ipv4, ipv6
	HostAddr string
	// DNS, IPv4, IPv6
	HostAddrType AddressType

	// used for whitelist
	// WARNING: Potentially overwrites the POD_IP from container env before passing to script in case of IPv4 or IPv6 in a dual stack cluster
	PodIP string
	// IPv4 or IPv6
	PodIPType AddressType
}

func retrieveHostInfo(fqHostname string, hostIPs, peers sets.Set[string]) (*HostInfo, error) {
	var info HostInfo
	var err error
	switch AddressType(*addrType) {
	case AddressTypeDNS:
		info.HostAddr = fqHostname
		info.HostAddrType = AddressTypeDNS
		info.PodIP = os.Getenv("POD_IP") // set using Downward api
		info.PodIPType, err = IPType(info.PodIP)
		if err != nil {
			return nil, err
		}
	case AddressTypeIP:
		hostAddrs := sets.List(peers.Intersection(hostIPs))
		if len(hostAddrs) == 0 {
			return nil, fmt.Errorf("none of the hostIPs %q found in peers %q", strings.Join(sets.List(hostIPs), ","), strings.Join(sets.List(peers), ","))
		}
		info.HostAddr = hostAddrs[0]
		info.HostAddrType, err = IPType(info.HostAddr)
		if err != nil {
			return nil, err
		}
		info.PodIP = info.HostAddr
		info.PodIPType = info.HostAddrType
	case AddressTypeIPv4:
		hostAddrs := sets.List(peers.Intersection(hostIPs))
		if len(hostAddrs) == 0 {
			return nil, fmt.Errorf("none of the hostIPs %q found in peers %q", strings.Join(sets.List(hostIPs), ","), strings.Join(sets.List(peers), ","))
		}
		info.HostAddr = hostAddrs[0]
		info.HostAddrType = AddressTypeIPv4
		info.PodIP = info.HostAddr
		info.PodIPType = info.HostAddrType
	case AddressTypeIPv6:
		hostAddrs := sets.List(peers.Intersection(hostIPs))
		if len(hostAddrs) == 0 {
			return nil, fmt.Errorf("none of the hostIPs %q found in peers %q", strings.Join(sets.List(hostIPs), ","), strings.Join(sets.List(peers), ","))
		}
		info.HostAddr = hostAddrs[0]
		info.HostAddrType = AddressTypeIPv6
		info.PodIP = info.HostAddr
		info.PodIPType = info.HostAddrType
	}
	return &info, nil
}

func IPType(s string) (AddressType, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return "", fmt.Errorf("%s is not a valid IP", s)
	}
	if strings.ContainsRune(s, ':') {
		return AddressTypeIPv6, nil
	}
	return AddressTypeIPv4, nil
}

func forwardSigterm() <-chan struct{} {
	shutdownHandler := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())

	signal.Notify(shutdownHandler, syscall.SIGTERM)
	go func() {
		<-shutdownHandler

		pgid, err := syscall.Getpgid(os.Getpid())
		if err != nil {
			log.Error(err, "failed to retrieve pgid for process", "pid", os.Getpid())
		} else {
			log.Info("sending SIGTERM", "pgid", pgid)
			err = syscall.Kill(-pgid, syscall.SIGTERM)
			if err != nil {
				log.Error(err, "failed to send SIGTERM", "pgid", pgid)
			}
		}
		cancel()

		fmt.Println("waiting for all child process to complete for SIGTERM")
		<-shutdownHandler
	}()

	return ctx.Done()
}

func main() {
	klog.InitFlags(nil)
	_ = flag.Set("v", "3")
	flag.Parse()

	stopCh := forwardSigterm()

	// TODO: Exit if there's no on-change?
	if err := run(stopCh); err != nil {
		log.Error(err, "peer finder exiting")
	}
	klog.Flush()

	log.Info("Block until Kubernetes sends SIGKILL")
	select {}
}

func run(stopCh <-chan struct{}) error {
	ns := *namespace
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %s", err)
	}
	var domainName string

	// If domain is not provided, try to get it from resolv.conf
	if *domain == "" {
		resolvConfBytes, err := os.ReadFile("/etc/resolv.conf")
		if err != nil {
			return fmt.Errorf("unable to read /etc/resolv.conf")
		}
		resolvConf := string(resolvConfBytes)

		var re *regexp.Regexp
		if ns == "" {
			// Looking for a domain that looks like with *.svc.**
			re, err = regexp.Compile(`\A(.*\n)*search\s{1,}(.*\s{1,})*(?P<goal>[a-zA-Z0-9-]{1,63}.svc.([a-zA-Z0-9-]{1,63}\.)*[a-zA-Z0-9]{2,63})`)
		} else {
			// Looking for a domain that looks like svc.**
			re, err = regexp.Compile(`\A(.*\n)*search\s{1,}(.*\s{1,})*(?P<goal>svc.([a-zA-Z0-9-]{1,63}\.)*[a-zA-Z0-9]{2,63})`)
		}
		if err != nil {
			return fmt.Errorf("failed to create regular expression: %v", err)
		}

		groupNames := re.SubexpNames()
		result := re.FindStringSubmatch(resolvConf)
		for k, v := range result {
			if groupNames[k] == "goal" {
				if ns == "" {
					// Domain is complete if ns is empty
					domainName = v
				} else {
					// Need to convert svc.** into ns.svc.**
					domainName = ns + "." + v
				}
				break
			}
		}
		log.Info("determined", "domain", domainName)

	} else {
		domainName = strings.Join([]string{ns, "svc", *domain}, ".")
	}

	if (*selector == "" && *svc == "") || domainName == "" || (*onChange == "" && *onStart == "") {
		return fmt.Errorf("incomplete args, require -on-change and/or -on-start, -service and -ns or an env var for POD_NAMESPACE")
	}

	if *selector != "" {
		config, err := clientcmd.BuildConfigFromFlags(*masterURL, *kubeconfigPath)
		if err != nil {
			return fmt.Errorf("could not get Kubernetes config: %s", err)
		}
		kc, err = kubernetes.NewForConfig(config)
		if err != nil {
			return err
		}
		RunHostAliasSyncer(kc, ns, *selector, *addrType, stopCh)
	}

	myName := strings.Join([]string{hostname, *svc, domainName}, ".")
	hostIPs, err := lookupHostIPs(hostname)
	if err != nil {
		return fmt.Errorf("failed to get ips from host %v", err)
	}
	script := *onStart
	if script == "" {
		script = *onChange
		log.Info(fmt.Sprintf("no on-start supplied, on-change %q will be applied on start.", script))
	}
	for peers := sets.New[string](); script != ""; time.Sleep(pollPeriod) {
		var newPeers sets.Set[string]
		if *selector != "" {
			newPeers, err = controller.listPodsIP()
			if err != nil {
				return err
			}
			if newPeers.Equal(peers) || !newPeers.HasAny(hostIPs.UnsortedList()...) {
				log.Info("have not found myself in list yet.", "hostname", myName, "hosts in list", strings.Join(sets.List(newPeers), ", "))
				continue
			}
		} else {
			newPeers, err = lookupDNS(*svc)
			if err != nil {
				log.Info(err.Error())
				continue
			}
			if newPeers.Equal(peers) || !newPeers.Has(myName) {
				log.Info("have not found myself in list yet.", "hostname", myName, "hosts in list", strings.Join(sets.List(newPeers), ", "))
				continue
			}
		}
		log.Info("peer list updated", "was", sets.List(peers), "now", sets.List(newPeers))

		// add extra newline at the end to ensure end of line for bash read command
		err = shellOut(script, newPeers, hostIPs, myName)
		if err != nil {
			return err
		}
		peers = newPeers
		script = *onChange
	}
	return nil
}
