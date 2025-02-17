/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package discovery

import (
	"fmt"
	"metacontroller/pkg/logging"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

type APIResource struct {
	metav1.APIResource
	APIVersion     string
	subresourceMap map[string]bool
}

func (r *APIResource) GroupVersion() schema.GroupVersion {
	gv, err := schema.ParseGroupVersion(r.APIVersion)
	if err != nil {
		// This shouldn't happen because we get this value from discovery.
		panic(fmt.Sprintf("API discovery returned invalid group/version %q: %v", r.APIVersion, err))
	}
	return gv
}

func (r *APIResource) GroupVersionKind() schema.GroupVersionKind {
	return r.GroupVersion().WithKind(r.Kind)
}

func (r *APIResource) GroupVersionResource() schema.GroupVersionResource {
	return r.GroupVersion().WithResource(r.Name)
}

func (r *APIResource) GroupResource() schema.GroupResource {
	return schema.GroupResource{Group: r.Group, Resource: r.Name}
}

func (r *APIResource) HasSubresource(subresourceKey string) bool {
	return r.subresourceMap[subresourceKey]
}

type groupVersionEntry struct {
	resources, kinds, subresources map[string]*APIResource
}

type ResourceMap struct {
	mutex         sync.RWMutex
	groupVersions map[string]groupVersionEntry

	discoveryClient discovery.DiscoveryInterface
	stopCh, doneCh  chan struct{}
}

func (rm *ResourceMap) Get(apiVersion, resource string) (result *APIResource) {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	gv, ok := rm.groupVersions[apiVersion]
	if !ok {
		return nil
	}
	return gv.resources[resource]
}

func (rm *ResourceMap) GetKind(apiVersion, kind string) (result *APIResource) {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()

	gv, ok := rm.groupVersions[apiVersion]
	if !ok {
		return nil
	}
	return gv.kinds[kind]
}

func (rm *ResourceMap) refresh() {
	// Fetch all API Group-Versions and their resources from the server.
	// We do this before acquiring the lock so we don't block readers.
	logging.Logger.V(7).Info("Refreshing API discovery info")
	_, groups, err := rm.discoveryClient.ServerGroupsAndResources()
	if err != nil {
		logging.Logger.Error(err, "Failed to fetch discovery info")
		return
	}

	// Denormalize resource lists into maps for convenient lookup
	// by either Group-Version-Kind or Group-Version-Resource.
	groupVersions := make(map[string]groupVersionEntry, len(groups))
	for _, group := range groups {
		gv, err := schema.ParseGroupVersion(group.GroupVersion)
		if err != nil {
			// This shouldn't happen because we get these values from the server.
			panic(fmt.Errorf("received invalid GroupVersion from server: %w", err))
		}
		gve := groupVersionEntry{
			resources:    make(map[string]*APIResource, len(group.APIResources)),
			kinds:        make(map[string]*APIResource, len(group.APIResources)),
			subresources: make(map[string]*APIResource, len(group.APIResources)),
		}

		for i := range group.APIResources {
			apiResource := &APIResource{
				APIResource: group.APIResources[i],
				APIVersion:  group.GroupVersion,
			}
			// Materialize default values from the list into each entry.
			if apiResource.Group == "" {
				apiResource.Group = gv.Group
			}
			if apiResource.Version == "" {
				apiResource.Version = gv.Version
			}
			gve.resources[apiResource.Name] = apiResource
			// Remember which resources are subresources, and map the kind to the main resource.
			// This is different from what RESTMapper provides because we already know
			// the full GroupVersionKind and just need the resource name.
			if strings.ContainsRune(apiResource.Name, '/') {
				gve.subresources[apiResource.Name] = apiResource
			} else {
				gve.kinds[apiResource.Kind] = apiResource
			}
		}

		// Group all subresources for a resource.
		for apiSubresourceName := range gve.subresources {
			arr := strings.Split(apiSubresourceName, "/")
			apiResourceName := arr[0]
			subresourceKey := arr[1]
			apiResource := gve.resources[apiResourceName]
			if apiResource == nil {
				continue
			}
			if apiResource.subresourceMap == nil {
				apiResource.subresourceMap = make(map[string]bool)
			}
			apiResource.subresourceMap[subresourceKey] = true
		}

		groupVersions[group.GroupVersion] = gve
	}

	// Replace the local cache.
	rm.mutex.Lock()
	rm.groupVersions = groupVersions
	rm.mutex.Unlock()
}

func (rm *ResourceMap) Start(refreshInterval time.Duration) {
	rm.stopCh = make(chan struct{})
	rm.doneCh = make(chan struct{})

	go func() {
		defer close(rm.doneCh)

		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()

		for {
			rm.refresh()

			select {
			case <-rm.stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (rm *ResourceMap) Stop() {
	close(rm.stopCh)
	<-rm.doneCh
}

func (rm *ResourceMap) HasSynced() bool {
	rm.mutex.RLock()
	defer rm.mutex.RUnlock()
	return rm.groupVersions != nil
}

func NewResourceMap(discoveryClient discovery.DiscoveryInterface) *ResourceMap {
	return &ResourceMap{
		discoveryClient: discoveryClient,
	}
}
