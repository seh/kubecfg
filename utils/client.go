// Copyright 2017 The kubecfg authors
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package utils

import (
	"fmt"
	"strings"
	"sync"

	openapi_v2 "github.com/googleapis/gnostic/OpenAPIv2"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/kubectl/cmd/util/openapi"
)

type memcachedDiscoveryClient struct {
	cl              discovery.DiscoveryInterface
	lock            sync.RWMutex
	servergroups    *metav1.APIGroupList
	serverresources map[string]*metav1.APIResourceList
	schemas         map[string]openapi.Resources
	schema          *openapi_v2.Document
}

// NewMemcachedDiscoveryClient creates a new DiscoveryClient that
// caches results in memory
func NewMemcachedDiscoveryClient(cl discovery.DiscoveryInterface) discovery.CachedDiscoveryInterface {
	c := &memcachedDiscoveryClient{cl: cl}
	c.Invalidate()
	return c
}

func (c *memcachedDiscoveryClient) Fresh() bool {
	return true
}

func (c *memcachedDiscoveryClient) Invalidate() {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.servergroups = nil
	c.serverresources = make(map[string]*metav1.APIResourceList)
	c.schemas = make(map[string]openapi.Resources)
}

func (c *memcachedDiscoveryClient) RESTClient() rest.Interface {
	return c.cl.RESTClient()
}

func (c *memcachedDiscoveryClient) ServerGroups() (*metav1.APIGroupList, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	var err error
	if c.servergroups != nil {
		return c.servergroups, nil
	}
	c.servergroups, err = c.cl.ServerGroups()
	return c.servergroups, err
}

func (c *memcachedDiscoveryClient) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	var err error
	if v := c.serverresources[groupVersion]; v != nil {
		return v, nil
	}
	c.serverresources[groupVersion], err = c.cl.ServerResourcesForGroupVersion(groupVersion)
	return c.serverresources[groupVersion], err
}

func (c *memcachedDiscoveryClient) ServerResources() ([]*metav1.APIResourceList, error) {
	return c.cl.ServerResources()
}

func (c *memcachedDiscoveryClient) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return c.cl.ServerPreferredResources()
}

func (c *memcachedDiscoveryClient) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return c.cl.ServerPreferredNamespacedResources()
}

func (c *memcachedDiscoveryClient) ServerVersion() (*version.Info, error) {
	return c.cl.ServerVersion()
}

func (c *memcachedDiscoveryClient) OpenAPISchema() (*openapi_v2.Document, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.schema != nil {
		return c.schema, nil
	}

	schema, err := c.cl.OpenAPISchema()
	if err != nil {
		return nil, err
	}

	c.schema = schema
	return schema, nil
}

var _ discovery.CachedDiscoveryInterface = &memcachedDiscoveryClient{}

type ClientPool struct {
	// TODO(seh): Add a lock and a map for a cache.
	config              *rest.Config
	apiPathResolverFunc dynamic.APIPathResolverFunc
}

func NewClientPool(config *rest.Config, apiPathResolverFunc dynamic.APIPathResolverFunc) *ClientPool {
	configCopy := *config
	return &ClientPool{
		config:              &configCopy,
		apiPathResolverFunc: apiPathResolverFunc,
	}
}

// TODO(seh): Restore the pool's ability to reuse clients.
func (p *ClientPool) ClientForGroupVersionKind(kind schema.GroupVersionKind) (dynamic.Interface, error) {
	gv := kind.GroupVersion()

	// TODO(seh): Look for client in cache.

	configCopy := *p.config
	config := &configCopy

	config.APIPath = p.apiPathResolverFunc(kind)
	config.GroupVersion = &gv

	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	// TODO(seh): Cache client.
	return client, nil
}

// ClientForResource returns the ResourceClient for a given object, together with any subresources
// necessary to refer to the object as a resource.
func ClientForResource(pool *ClientPool, disco discovery.DiscoveryInterface, obj runtime.Object, defNs string) (dynamic.ResourceInterface, []string, error) {
	gvk := obj.GetObjectKind().GroupVersionKind()

	client, err := pool.ClientForGroupVersionKind(gvk)
	if err != nil {
		return nil, nil, err
	}

	resource, err := serverResourceForGroupVersionKind(disco, gvk)
	if err != nil {
		return nil, nil, err
	}

	meta, err := meta.Accessor(obj)
	if err != nil {
		return nil, nil, err
	}
	namespace := meta.GetNamespace()
	if namespace == "" {
		namespace = defNs
	}

	log.Debugf("Fetching client for %s namespace=%s", resource, namespace)

	resourceTokens := strings.SplitN(resource.Name, "/", 2)
	name := resourceTokens[0]
	var subresources []string
	if len(resourceTokens) > 1 {
		subresources = strings.Split(resourceTokens[1], "/")
	}
	rc := client.Resource(gvk.GroupVersion().WithResource(name)).Namespace(namespace)
	return rc, subresources, nil
}

func serverResourceForGroupVersionKind(disco discovery.ServerResourcesInterface, gvk schema.GroupVersionKind) (*metav1.APIResource, error) {
	resources, err := disco.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return nil, fmt.Errorf("unable to fetch resource description for %s: %v", gvk.GroupVersion(), err)
	}

	for _, r := range resources.APIResources {
		if r.Kind == gvk.Kind {
			log.Debugf("Using resource '%s' for %s", r.Name, gvk)
			return &r, nil
		}
	}

	return nil, fmt.Errorf("Server is unable to handle %s", gvk)
}
