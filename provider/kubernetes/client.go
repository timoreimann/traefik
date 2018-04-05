package kubernetes

import (
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/containous/traefik/log"
	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

const resyncPeriod = 10 * time.Minute

type resourceEventHandler struct {
	ev chan<- interface{}
}

func (reh *resourceEventHandler) OnAdd(obj interface{}) {
	eventHandlerFunc(reh.ev, obj)
}

func (reh *resourceEventHandler) OnUpdate(oldObj, newObj interface{}) {
	eventHandlerFunc(reh.ev, newObj)
}

func (reh *resourceEventHandler) OnDelete(obj interface{}) {
	eventHandlerFunc(reh.ev, obj)
}

// Client is a client for the Provider master.
// WatchAll starts the watch of the Provider resources and updates the stores.
// The stores can then be accessed via the Get* functions.
type Client interface {
	WatchAll(namespaces Namespaces, labelSelector string, stopCh <-chan struct{}) (<-chan interface{}, error)
	GetIngresses() []*extensionsv1beta1.Ingress
	GetService(namespace, name string) (*corev1.Service, bool, error)
	GetSecret(namespace, name string) (*corev1.Secret, bool, error)
	GetEndpoints(namespace, name string) (*corev1.Endpoints, bool, error)
}

type clientImpl struct {
	clientset        *kubernetes.Clientset
	ingressFactories map[string]informers.SharedInformerFactory
	otherFactories   map[string]informers.SharedInformerFactory
	isNamespaceAll   bool
}

func newClientImpl(clientset *kubernetes.Clientset) Client {
	return &clientImpl{
		clientset:        clientset,
		ingressFactories: make(map[string]informers.SharedInformerFactory),
		otherFactories:   make(map[string]informers.SharedInformerFactory),
	}
}

// NewInClusterClient returns a new Provider client that is expected to run
// inside the cluster.
func NewInClusterClient(endpoint string) (Client, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster configuration: %s", err)
	}

	if endpoint != "" {
		config.Host = endpoint
	}

	return createClientFromConfig(config)
}

// NewExternalClusterClient returns a new Provider client that may run outside
// of the cluster.
// The endpoint parameter must not be empty.
func NewExternalClusterClient(endpoint, token, caFilePath string) (Client, error) {
	if endpoint == "" {
		return nil, errors.New("endpoint missing for external cluster client")
	}

	config := &rest.Config{
		Host:        endpoint,
		BearerToken: token,
	}

	if caFilePath != "" {
		caData, err := ioutil.ReadFile(caFilePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file %s: %s", caFilePath, err)
		}

		config.TLSClientConfig = rest.TLSClientConfig{CAData: caData}
	}

	return createClientFromConfig(config)
}

func createClientFromConfig(c *rest.Config) (Client, error) {
	clientset, err := kubernetes.NewForConfig(c)
	if err != nil {
		return nil, err
	}

	return newClientImpl(clientset), nil
}

// WatchAll starts namespace-specific controllers for all relevant kinds.
func (c *clientImpl) WatchAll(namespaces Namespaces, labelSelector string, stopCh <-chan struct{}) (<-chan interface{}, error) {
	eventCh := make(chan interface{}, 1)

	_, err := labels.Parse(labelSelector)
	if err != nil {
		return nil, err
	}

	if len(namespaces) == 0 {
		namespaces = Namespaces{metav1.NamespaceAll}
		c.isNamespaceAll = true
	}

	eventHandler := newResourceEventHandler(eventCh)
	for _, ns := range namespaces {
		ingressFactory := informers.NewFilteredSharedInformerFactory(c.clientset, resyncPeriod, ns, func(opts *metav1.ListOptions) {
			opts.LabelSelector = labelSelector
		})
		otherFactory := informers.NewFilteredSharedInformerFactory(c.clientset, resyncPeriod, ns, nil)
		ingressFactory.Extensions().V1beta1().Ingresses().Informer().AddEventHandler(eventHandler)
		otherFactory.Core().V1().Services().Informer().AddEventHandler(eventHandler)
		otherFactory.Core().V1().Endpoints().Informer().AddEventHandler(eventHandler)

		c.ingressFactories[ns] = ingressFactory
		c.otherFactories[ns] = otherFactory
	}

	for _, ns := range namespaces {
		c.ingressFactories[ns].Start(stopCh)
		c.otherFactories[ns].Start(stopCh)
	}

	for _, ns := range namespaces {
		for t, ok := range c.ingressFactories[ns].WaitForCacheSync(stopCh) {
			if !ok {
				return nil, fmt.Errorf("timed out waiting for Ingress controller caches to sync %s in namespace %q", t.String(), ns)
			}
		}
		for t, ok := range c.otherFactories[ns].WaitForCacheSync(stopCh) {
			if !ok {
				return nil, fmt.Errorf("timed out waiting for non-Ingress controller caches to sync %s in namespace %q", t.String(), ns)
			}
		}
	}

	// Do not wait for the Secrets store to get synced since we cannot rely on
	// users having granted RBAC permissions for this object.
	// https://github.com/containous/traefik/issues/1784 should improve the
	// situation here in the future.
	for _, ns := range namespaces {
		c.otherFactories[ns].Core().V1().Secrets().Informer().AddEventHandler(eventHandler)
		c.otherFactories[ns].Start(stopCh)
	}

	return eventCh, nil
}

// GetIngresses returns all Ingresses for observed namespaces in the cluster.
func (c *clientImpl) GetIngresses() []*extensionsv1beta1.Ingress {
	var result []*extensionsv1beta1.Ingress
	for ns, factory := range c.ingressFactories {
		ings, err := factory.Extensions().V1beta1().Ingresses().Lister().List(labels.Everything())
		if err != nil {
			log.Errorf("Failed to list ingresses in namespace %s: %s", ns, err)
		}
		for _, ing := range ings {
			result = append(result, ing)
		}
	}
	return result
}

// GetService returns the named service from the given namespace.
func (c *clientImpl) GetService(namespace, name string) (*corev1.Service, bool, error) {
	var service *corev1.Service
	item, exists, err := c.otherFactories[c.lookupNamespace(namespace)].Core().V1().Services().Informer().GetStore().GetByKey(namespace + "/" + name)
	if item != nil {
		service = item.(*corev1.Service)
	}
	return service, exists, err
}

// GetEndpoints returns the named endpoints from the given namespace.
func (c *clientImpl) GetEndpoints(namespace, name string) (*corev1.Endpoints, bool, error) {
	var endpoint *corev1.Endpoints
	item, exists, err := c.otherFactories[c.lookupNamespace(namespace)].Core().V1().Endpoints().Informer().GetStore().GetByKey(namespace + "/" + name)
	if item != nil {
		endpoint = item.(*corev1.Endpoints)
	}
	return endpoint, exists, err
}

// GetSecret returns the named secret from the given namespace.
func (c *clientImpl) GetSecret(namespace, name string) (*corev1.Secret, bool, error) {
	var secret *corev1.Secret
	item, exists, err := c.otherFactories[c.lookupNamespace(namespace)].Core().V1().Secrets().Informer().GetStore().GetByKey(namespace + "/" + name)
	if err == nil && item != nil {
		secret = item.(*corev1.Secret)
	}
	return secret, exists, err
}

// lookupNamespace returns the lookup namespace key for the given namespace.
// When listening on all namespaces, it returns the client-go identifier ("")
// for all-namespaces. Otherwise, it returns the given namespace.
// The distinction is necessary because we index all informers on the special
// identifier iff all-namespaces are requested but receive specific namespace
// identifiers from the Kubernetes API, so we have to bridge this gap.
func (c *clientImpl) lookupNamespace(ns string) string {
	if c.isNamespaceAll {
		return metav1.NamespaceAll
	}
	return ns
}

func newResourceEventHandler(events chan<- interface{}) cache.ResourceEventHandler {
	return &resourceEventHandler{events}
}

// eventHandlerFunc will pass the obj on to the events channel or drop it.
// This is so passing the events along won't block in the case of high volume.
// The events are only used for signalling anyway so dropping a few is ok.
func eventHandlerFunc(events chan<- interface{}, obj interface{}) {
	select {
	case events <- obj:
	default:
	}
}
