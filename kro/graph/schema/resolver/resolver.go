// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resolver

import (
	"net/http"
	"time"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// NewCombinedResolver creates a new schema resolver that can resolve both core and client types.
func NewCombinedResolver(clientConfig *rest.Config, httpClient *http.Client) (resolver.SchemaResolver, error) {
	// Create a regular discovery client first
	discoveryClient, err := discovery.NewDiscoveryClientForConfigAndClient(clientConfig, httpClient)
	if err != nil {
		return nil, err
	}

	// ClientResolver is a resolver that uses the discovery client to resolve
	// client types. It is used to resolve types that are not known at compile
	// time a.k.a present in:
	// https://github.com/kubernetes/apiextensions-apiserver/blob/master/pkg/generated/openapi/zz_generated.openapi.go
	clientResolver := &resolver.ClientDiscoveryResolver{
		Discovery: discoveryClient,
	}

	cachedResolver := NewTTLCachedSchemaResolver(
		clientResolver,
		500,           // maxSize: enough for 200 CRDs × 2-3 versions
		5*time.Minute, // TTL: balance between freshness and performance
	)

	// CoreResolver is a resolver that uses the OpenAPI definitions to resolve
	// core types. It is used to resolve types that are known at compile time.
	coreResolver := resolver.NewDefinitionsSchemaResolver(
		openapi.GetOpenAPIDefinitions,
		scheme.Scheme,
	)

	combinedResolver := coreResolver.Combine(cachedResolver)
	return combinedResolver, nil
}

// NewCombinedResolverFromSchemas creates a schema resolver that combines
// a schema map resolver (for Crossplane-provided schemas) with a core resolver
// (for built-in Kubernetes types). This is the primary constructor for use
// with Crossplane functions that receive OpenAPI schemas via required_schemas.
func NewCombinedResolverFromSchemas(schemaMapResolver *SchemaMapResolver) resolver.SchemaResolver {
	coreResolver := newCoreResolver()
	// Combine: schema map first (Crossplane-provided), then core (built-in types).
	return schemaMapResolver.Combine(coreResolver)
}

// NewCombinedResolverFromCRDs creates a schema resolver that combines
// a CRD schema resolver (for schemas extracted from CRDs) with a core resolver
// (for built-in Kubernetes types). This is the constructor for use with
// Crossplane functions that receive CRDs via extra_resources.
func NewCombinedResolverFromCRDs(crds []*extv1.CustomResourceDefinition) (resolver.SchemaResolver, error) {
	crdResolver, err := NewCRDSchemaResolver(crds)
	if err != nil {
		return nil, err
	}

	coreResolver := newCoreResolver()

	// Combine: CRD resolver first, then core (built-in types).
	// We wrap the CRD resolver to implement the Combine pattern.
	return &combinedResolver{
		primary:  crdResolver,
		fallback: coreResolver,
	}, nil
}

// newCoreResolver creates a resolver for built-in Kubernetes types using
// compiled-in OpenAPI definitions. This handles types like Deployment, Service,
// ConfigMap, etc. that are part of the core Kubernetes API.
func newCoreResolver() resolver.SchemaResolver {
	return resolver.NewDefinitionsSchemaResolver(
		openapi.GetOpenAPIDefinitions,
		scheme.Scheme,
	)
}
