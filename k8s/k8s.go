package k8s

import (
	"fmt"
	"time"

	kubelet "github.com/wish/katalog-sync/pkg/daemon"

	"github.com/jtblin/kube2iam"
	"github.com/jtblin/kube2iam/metrics"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	selector "k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const (
	podIPIndexName     = "byPodIP"
	namespaceIndexName = "byName"
)

// Client represents a kubernetes client.
type Client struct {
	*kubernetes.Clientset
	namespaceController cache.Controller
	namespaceIndexer    cache.Indexer
	podController       cache.Controller
	podIndexer          cache.Indexer
	nodeName            string
	useAPIOnCacheIssues bool
}

// Returns a cache.ListWatch that gets all changes to pods.
func (k8s *Client) createPodLW() *cache.ListWatch {
	fieldSelector := selector.Everything()
	if k8s.nodeName != "" {
		fieldSelector = selector.OneTermEqualSelector("spec.nodeName", k8s.nodeName)
	}
	return cache.NewListWatchFromClient(k8s.CoreV1().RESTClient(), "pods", v1.NamespaceAll, fieldSelector)
}

// WatchForPods watches for pod changes.
func (k8s *Client) WatchForPods(podEventLogger cache.ResourceEventHandler, resyncPeriod time.Duration) cache.InformerSynced {
	k8s.podIndexer, k8s.podController = cache.NewIndexerInformer(
		k8s.createPodLW(),
		&v1.Pod{},
		resyncPeriod,
		podEventLogger,
		cache.Indexers{podIPIndexName: kube2iam.PodIPIndexFunc},
	)
	go k8s.podController.Run(wait.NeverStop)
	return k8s.podController.HasSynced
}

// returns a cache.ListWatch of namespaces.
func (k8s *Client) createNamespaceLW() *cache.ListWatch {
	return cache.NewListWatchFromClient(k8s.CoreV1().RESTClient(), "namespaces", v1.NamespaceAll, selector.Everything())
}

// WatchForNamespaces watches for namespaces changes.
func (k8s *Client) WatchForNamespaces(nsEventLogger cache.ResourceEventHandler, resyncPeriod time.Duration) cache.InformerSynced {
	k8s.namespaceIndexer, k8s.namespaceController = cache.NewIndexerInformer(
		k8s.createNamespaceLW(),
		&v1.Namespace{},
		resyncPeriod,
		nsEventLogger,
		cache.Indexers{namespaceIndexName: kube2iam.NamespaceIndexFunc},
	)
	go k8s.namespaceController.Run(wait.NeverStop)
	return k8s.namespaceController.HasSynced
}

// ListPodIPs returns the underlying set of pods being managed/indexed
func (k8s *Client) ListPodIPs() []string {
	// Decided to simply dump this and leave it up to consumer
	// as k8s package currently doesn't need to be concerned about what's
	// a signficant annotation to process, that is left up to store/server
	return k8s.podIndexer.ListIndexFuncValues(podIPIndexName)
}

// ListNamespaces returns the underlying set of namespaces being managed/indexed
func (k8s *Client) ListNamespaces() []string {
	return k8s.namespaceIndexer.ListIndexFuncValues(namespaceIndexName)
}

// PodByIP provides the representation of the pod itself being cached keyed off of it's IP
// Returns an error if there are multiple pods attempting to be keyed off of the same IP
// (Which happens when they of type `hostNetwork: true`)
func (k8s *Client) PodByIP(IP string) (*v1.Pod, error) {
	pods, err := k8s.podIndexer.ByIndex(podIPIndexName, IP)
	if err != nil {
		return nil, err
	}

	if len(pods) == 1 {
		return pods[0].(*v1.Pod), nil
	}

	if len(pods) == 0 {
		metrics.PodNotFoundInCache.Inc()
		if k8s.useAPIOnCacheIssues {
			pod, err := getPodFromAPIByIP(k8s, IP)
			if err != nil {
				return nil, err
			}
			return pod, nil
		}
		errMsg := fmt.Errorf("pod with specificed IP not found")
		log.Error(errMsg)
		return nil, errMsg
	}

	// more than one pod in the cache
	if k8s.useAPIOnCacheIssues {
		pod, err := getPodFromAPIByIP(k8s, IP)
		if err != nil {
			return nil, err
		}
		return pod, nil
	}
	podNames := make([]string, len(pods))
	for i, pod := range pods {
		podNames[i] = pod.(*v1.Pod).ObjectMeta.Name
	}
	errMsg := fmt.Errorf("%d pods (%v) with the ip %s indexed", len(pods), podNames, IP)
	log.Error(errMsg)
	return nil, errMsg

}

// getPodFromAPIByIP queries the k8s api server trying to make a decision based on NON cached data
// If the indexed pods all have HostNetwork = true the function return nil and the error message.
// If we retrive a running pod that doesn't have HostNetwork = true and it is in Running state will return that.
func getPodFromAPIByIP(k8s *Client, IP string) (*v1.Pod, error) {
	log.Infof("getPodFromAPIByIP: kubelet Searching IP %s for: spec.nodeName==%s", IP, k8s.nodeName)
	kubeletClient, err := kubelet.NewKubeletClient(kubelet.KubeletClientConfig{APIEndpoint: "http://localhost:10255/pods", InsecureSkipVerify: true})
	if err != nil {
		log.Errorf("Unable to create kubelet client: %v", err)
	}

	runningPodList, err := kubeletClient.GetPodList()
	metrics.K8sAPIReqCount.Inc()
	if err != nil {
		errMsg := fmt.Errorf("getPodFromAPIByIP: Error retriving the pod with IP %s from the k8s kubelet %s", IP, err)
		log.Error(errMsg)
		return nil, errMsg
	}

	for _, pod := range runningPodList.Items {
		log.Infof("getPodFromAPIByIP: kubelet Found container %s with IP: %s", pod.GetName(), pod.Status.PodIP)
		if pod.Spec.HostNetwork {
			continue
		}
		if pod.Status.PodIP == IP {
			log.Infof("getPodFromAPIByIP: kubelet Returning %s", pod.GetName())
			metrics.K8sAPIReqSuccesCount.Inc()
			return &pod, nil
		}
	}

	errMsg := fmt.Errorf("more than a pod with the same IP has been indexed, this can happen when pods have hostNetwork: true")
	log.Error(errMsg)
	return nil, errMsg
}

// NamespaceByName retrieves a namespace by it's given name.
// Returns an error if there are no namespaces available
func (k8s *Client) NamespaceByName(namespaceName string) (*v1.Namespace, error) {
	namespace, err := k8s.namespaceIndexer.ByIndex(namespaceIndexName, namespaceName)
	if err != nil {
		return nil, err
	}

	if len(namespace) == 0 {
		errMsg := fmt.Errorf("namespace was not found")
		log.Error(errMsg)
		return nil, errMsg
	}

	return namespace[0].(*v1.Namespace), nil
}

// NewClient returns a new kubernetes client.
func NewClient(host, token, nodeName string, insecure, UseAPIOnCacheIssues bool) (*Client, error) {
	var config *rest.Config
	var err error
	if host != "" && token != "" {
		config = &rest.Config{
			Host:        host,
			BearerToken: token,
			TLSClientConfig: rest.TLSClientConfig{
				Insecure: insecure,
			},
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Client{Clientset: client, nodeName: nodeName, useAPIOnCacheIssues: UseAPIOnCacheIssues}, nil
}
