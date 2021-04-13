/*
Copyright AppsCode Inc. and Contributors

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

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	addrType  string
	namespace string

	indexer  cache.Indexer
	queue    workqueue.RateLimitingInterface
	informer cache.Controller
	lister   corelisters.PodLister
}

// NewController creates a new Controller.
func NewController(queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller, namespace, addrType string) *Controller {
	return &Controller{
		informer:  informer,
		indexer:   indexer,
		queue:     queue,
		lister:    corelisters.NewPodLister(indexer),
		namespace: namespace,
		addrType:  addrType,
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	// Invoke the method containing the business logic
	err := c.reconcile(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

// reconcile is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) reconcile(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		log.Error(err, "Fetching object from store failed", "pod", key)
		return err
	}

	if !exists {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		log.Info("Pod does not exist anymore", "pod", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		log.Info("Sync/Add/Update for Pod", "name", obj.(*core.Pod).GetName())
	}

	return c.syncHostsFile()
}

func (c *Controller) syncHostsFile() error {
	aliases, err := c.GenerateAliases()
	if err != nil {
		log.Error(err, "Failed to generate aliases")
		return err
	}
	updated, err := UpdateHostsFile(*hostsFilePath, aliases)
	if err != nil {
		log.Error(err, "failed to update host aliases", "hostsFilePath", *hostsFilePath, "aliases", aliases)
		return err
	}
	log.Info("host alias synced", "hostsFilePath", *hostsFilePath, "updated", updated)
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		log.Error(err, "Error syncing", "pod", key)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	log.Error(err, "Dropping pod out of the queue", "pod", key)
}

// Run begins watching and syncing.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	log.Info("Starting Pod controller")

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
		return
	}

	_ = c.syncHostsFile()

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Info("Stopping Pod controller")
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func RunHostAliasSyncer(kc kubernetes.Interface, namespace, sel, addrType string, stop <-chan struct{}) {
	// create the pod watcher
	podListWatcher := cache.NewFilteredListWatchFromClient(kc.CoreV1().RESTClient(), "pods", namespace, func(options *metav1.ListOptions) {
		options.LabelSelector = sel
	})

	// create the workqueue
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	// Bind the workqueue to a cache with the help of an informer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Pod than the version which was responsible for triggering the update.
	indexer, informer := cache.NewIndexerInformer(podListWatcher, &core.Pod{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
	}, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})

	controller = NewController(queue, indexer, informer, namespace, addrType)

	// Now let's start the ctrl
	go controller.Run(1, stop)
}

type IPNotAssigned struct {
	name string
}

var _ error = IPNotAssigned{}

func (e IPNotAssigned) Error() string {
	return fmt.Sprintf("ip not assigned to pod %s", e.name)
}

type IPNotFound struct {
	addrType string
	name     string
}

var _ error = IPNotFound{}

func (e IPNotFound) Error() string {
	return fmt.Sprintf("%s address not found for pod %s", e.addrType, e.name)
}

func GetIP(pod *core.Pod, addrType string) (string, error) {
	addrs := pod.Status.PodIPs
	if len(addrs) == 0 {
		addrs = []core.PodIP{{IP: pod.Status.PodIP}}
	}
	if len(addrs) == 0 {
		return "", IPNotAssigned{name: pod.Name}
	}

	switch addrType {
	case "IP":
		return addrs[0].IP, nil
	case "IPv4":
		for _, addr := range addrs {
			if ip := net.ParseIP(addr.IP); ip != nil && !strings.ContainsRune(addr.IP, ':') {
				return addr.IP, nil
			}
		}
	case "IPv6":
		for _, addr := range addrs {
			if ip := net.ParseIP(addr.IP); ip != nil && strings.ContainsRune(addr.IP, ':') {
				return addr.IP, nil
			}
		}
	}

	return "", IPNotFound{
		addrType: addrType,
		name:     pod.Name,
	}
}

func (c *Controller) GenerateAliases() (string, error) {
	pods, err := c.lister.Pods(c.namespace).List(labels.Everything())
	if err != nil {
		return "", err
	}

	peers := make([]string, 0, len(pods))
	for _, pod := range pods {
		ip, err := GetIP(pod, c.addrType)
		if err != nil {
			return "", err
		}
		peers = append(peers, fmt.Sprintf("%s %s", ip, pod.Name))
	}
	sort.Strings(peers)
	return strings.Join(peers, "\n"), nil
}

func (c *Controller) listPodsIP() (sets.String, error) {
	pods, err := c.lister.Pods(c.namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	endpoints := sets.NewString()
	for _, pod := range pods {
		ip, err := GetIP(pod, c.addrType)
		if err != nil {
			return nil, err
		}
		endpoints.Insert(ip)
	}

	return endpoints, nil
}

var re = regexp.MustCompile(`# peer-finder-managed-aliases:(.*)`)

const hostsfile = `# Kubernetes-managed hosts file.
127.0.0.1	localhost
::1	localhost ip6-localhost ip6-loopback
fe00::0	ip6-localnet
fe00::0	ip6-mcastprefix
fe00::1	ip6-allnodes
fe00::2	ip6-allrouters
`

func UpdateHostsFile(path, aliases string) (bool, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	matches := re.FindStringSubmatch(string(data))

	var curHex string
	if len(matches) == 2 {
		curHex = matches[1]
	}

	hash := sha1.Sum([]byte(aliases))
	newHex := hex.EncodeToString(hash[:])
	if curHex == newHex {
		return false, nil
	}

	var out bytes.Buffer
	out.WriteString(hostsfile)
	out.WriteRune('\n')
	out.WriteString(fmt.Sprintf("# peer-finder-managed-aliases:%s", newHex))
	out.WriteRune('\n')
	out.WriteString(aliases)

	err = ioutil.WriteFile(path, out.Bytes(), 0644)
	if err != nil {
		return false, err
	}
	return true, nil
}
