package builder

import (
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/rest"
	"github.com/tilt-dev/tilt-apiserver/pkg/storage/filepath"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Registers a request handler for the resource that stores it on the file system.
func (a *Server) WithResourceFileStorage(obj resource.Object, path string) *Server {
	fs := filepath.RealFS{}
	ws := filepath.NewWatchSet()
	strategy := rest.DefaultStrategy{
		Object:      obj,
		ObjectTyper: a.scheme,
	}
	a.WithResourceAndHandler(obj, filepath.NewJSONFilepathStorageProvider(obj, path, fs, ws, strategy))

	// automatically create status subresource if the object implements the status interface
	if _, ok := obj.(resource.ObjectWithStatusSubResource); ok {
		a.WithSubResourceAndHandler(obj, "status",
			filepath.NewJSONFilepathStorageProvider(obj, path, fs, ws, rest.StatusSubResourceStrategy{Strategy: strategy}))
	}
	return a
}

// Registers a request handler for the resource that stores it in memory.
func (a *Server) WithResourceMemoryStorage(obj resource.Object, path string) *Server {
	if a.memoryFS == nil {
		a.memoryFS = filepath.NewMemoryFS()
	}
	ws := filepath.NewWatchSet()
	strategy := rest.DefaultStrategy{
		Object:      obj,
		ObjectTyper: a.scheme,
	}
	a.WithResourceAndHandler(obj, filepath.NewJSONFilepathStorageProvider(obj, path, a.memoryFS, ws, strategy))

	// automatically create status subresource if the object implements the status interface
	if _, ok := obj.(resource.ObjectWithStatusSubResource); ok {
		a.WithSubResourceAndHandler(obj, "status",
			filepath.NewJSONFilepathStorageProvider(obj, path, a.memoryFS, ws, rest.StatusSubResourceStrategy{Strategy: strategy}))
	}
	return a
}

// WithResourceAndHandler registers a request handler for the resource rather than the default
// etcd backed storage.
//
// Note: WithResourceAndHandler should never be called after the GroupResource has already been registered with
// another version.
//
// Note: WithResourceAndHandler will NOT register the "status" subresource for the resource object.
func (a *Server) WithResourceAndHandler(obj resource.Object, sp rest.ResourceHandlerProvider) *Server {
	gvr := obj.GetGroupVersionResource()
	a.schemeBuilder.Register(resource.AddToScheme(obj))
	return a.forGroupVersionResource(gvr, sp)
}

// forGroupVersionResource manually registers storage for a specific resource or subresource version.
func (a *Server) forGroupVersionResource(
	gvr schema.GroupVersionResource, sp rest.ResourceHandlerProvider) *Server {
	// register the group version
	a.withGroupVersions(gvr.GroupVersion())

	// TODO: make sure folks don't register multiple storage instance for the same group-resource
	// don't replace the existing instance otherwise it will chain wrapped singletonProviders when
	// fetching from the map before calling this function
	if _, found := a.storage[gvr.GroupResource()]; !found {
		a.storage[gvr.GroupResource()] = &singletonProvider{Provider: sp}
	}

	// add the API with its storage
	a.apis[gvr] = sp
	return a
}

// WithSubResourceAndHandler registers a request handler for the subresource rather than the default
// etcd backed storage.
//
// Note: WithSubResource does NOT register the request or parent with the SchemeBuilder.  If they were not registered
// through a WithResource call, then this must be done manually with WithAdditionalSchemeInstallers.
func (a *Server) WithSubResourceAndHandler(
	parent resource.Object, subResourcePath string, sp rest.ResourceHandlerProvider) *Server {
	gvr := parent.GetGroupVersionResource()
	// add the subresource path
	gvr.Resource = gvr.Resource + "/" + subResourcePath
	return a.forGroupVersionResource(gvr, sp)
}

// WithSchemeInstallers registers functions to install resource types into the Scheme.
func (a *Server) withGroupVersions(versions ...schema.GroupVersion) *Server {
	if a.groupVersions == nil {
		a.groupVersions = map[schema.GroupVersion]bool{}
	}
	for _, gv := range versions {
		if _, found := a.groupVersions[gv]; found {
			continue
		}
		a.groupVersions[gv] = true
		a.orderedGroupVersions = append(a.orderedGroupVersions, gv)
	}
	return a
}
