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
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
)

const (
	pollPeriod = 1 * time.Second
	hostsPath  = "/etc/hosts"
)

var (
	kubeClient *kubernetes.Clientset
	log        = klogr.New().WithName("peer-finder")
)

func init() {
	kubeConfig, err := restclient.InClusterConfig()
	if err != nil {
		klog.Fatalln(err)
	}
	kubeClient, err = kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		klog.Fatalln(err)
	}
}

var (
	onChange  = flag.String("on-change", "", "Script to run on change, must accept a new line separated list of peers via stdin.")
	onStart   = flag.String("on-start", "", "Script to run on start, must accept a new line separated list of peers via stdin.")
	svc       = flag.String("service", "", "Governing service responsible for the DNS records of the domain this pod is in.")
	namespace = flag.String("ns", "", "The namespace this pod is running in. If unspecified, the POD_NAMESPACE env var is used.")
	domain    = flag.String("domain", "", "The Cluster Domain which is used by the Cluster, if not set tries to determine it from /etc/resolv.conf file.")
	selector  = flag.String("selector", "", "The selector is used to select the pods whose ip will use to form peers")
)

func lookupDNSs(svcName string) (sets.String, error) {
	endpoints := sets.NewString()
	_, srvRecords, err := net.LookupSRV("", "", svcName)
	if err != nil {
		return endpoints, err
	}
	for _, srvRecord := range srvRecords {
		// The SRV records ends in a "." for the root domain
		ep := fmt.Sprintf("%v", srvRecord.Target[:len(srvRecord.Target)-1])
		endpoints.Insert(ep)
	}
	return endpoints, nil
}

func lookupHostIPs(hostName string) (sets.String, error) {
	ips := sets.NewString()
	hostIPs, err := net.LookupIP(hostName)
	if err != nil {
		return nil, err
	}
	for _, hostIP := range hostIPs {
		ips.Insert(fmt.Sprintf("%v", hostIP.String()))
	}
	return ips, nil
}

func updateHostsFile(ipAliases []string) error {
	//Append second line
	file, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	var fileContent string
	for _, ipAlias := range ipAliases {
		fileContent += fmt.Sprintf("%s\n", ipAlias)
	}

	_, err = file.WriteString(fileContent)
	return err
}

func listPodsIP(namespace string) (sets.String, []string, error) {
	endpoints := sets.NewString()
	var ipAliases []string
	podList, err := kubeClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: *selector,
	})
	if err != nil {
		return nil, nil, err
	}
	for _, pod := range podList.Items {
		endpoints.Insert(pod.Status.PodIP)
		ipAliases = append(ipAliases, fmt.Sprintf("%s	%s", pod.Status.PodIP, pod.Name))
	}

	return endpoints, ipAliases, nil
}

func shellOut(sendStdin, script string) error {
	log.Info("exec", "command", script, "stdin", sendStdin)
	cmd := exec.Command(script)
	cmd.Stdin = strings.NewReader(sendStdin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Error(err, "execution failed", "script", script)
	}
	return err
}

func forwardSigterm() {
	shutdownHandler := make(chan os.Signal, 1)
	signal.Notify(shutdownHandler, syscall.SIGTERM)
	go func() {
		<-shutdownHandler

		pgid, err := syscall.Getpgid(os.Getpid())
		if err != nil {
			panic(err)
		}
		fmt.Println("sending SIGTERM to pgid", pgid)
		err = syscall.Kill(-pgid, syscall.SIGTERM)
		if err != nil {
			panic(err)
		}

		fmt.Println("waiting for all child process to complete for SIGTERM")
		<-shutdownHandler
	}()
}

func main() {
	klog.InitFlags(nil)
	_ = flag.Set("v", "3")
	flag.Parse()

	forwardSigterm()

	// TODO: Exit if there's no on-change?
	if err := run(); err != nil {
		log.Error(err, "peer finder exiting")
	}
	klog.Flush()

	log.Info("Block until Kubernetes sends SIGKILL")
	select {}
}

func run() error {
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
		resolvConfBytes, err := ioutil.ReadFile("/etc/resolv.conf")
		resolvConf := string(resolvConfBytes)
		if err != nil {
			return fmt.Errorf("unable to read /etc/resolv.conf")
		}

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
	for newPeers, peers := sets.NewString(), sets.NewString(); script != ""; time.Sleep(pollPeriod) {
		var ipAliases []string
		if *selector != "" {
			newPeers, ipAliases, err = listPodsIP(ns)
			if err != nil {
				return err
			}
			if newPeers.Equal(peers) || !newPeers.HasAny(hostIPs.List()...) {
				log.Info("have not found myself in list yet.", "hostname", myName, "hosts in list", strings.Join(newPeers.List(), ", "))
				continue
			}
			if err := updateHostsFile(ipAliases); err != nil {
				return fmt.Errorf("%s file is failed to update, reason: %v", hostsPath, err)
			}
		} else {
			newPeers, err = lookupDNSs(*svc)
			if err != nil {
				log.Info(err.Error())
				continue
			}
			if newPeers.Equal(peers) || !newPeers.Has(myName) {
				log.Info("have not found myself in list yet.", "hostname", myName, "hosts in list", strings.Join(newPeers.List(), ", "))
				continue
			}
		}
		peerList := newPeers.List()
		sort.Strings(peerList)
		log.Info("peer list updated", "was", peers.List(), "now", newPeers.List())
		err = shellOut(strings.Join(peerList, "\n"), script)
		if err != nil {
			return err
		}
		peers = newPeers
		script = *onChange
	}
	return nil
}
