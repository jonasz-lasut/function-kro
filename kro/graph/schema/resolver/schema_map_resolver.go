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
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// SchemaMapResolver is a resolver.SchemaResolver backed by a map of GVK to
// OpenAPI schemas. This is used when schemas are provided directly (e.g., from
// Crossplane's required_schemas) rather than extracted from CRDs.
type SchemaMapResolver struct {
	schemas map[schema.GroupVersionKind]*spec.Schema
	mx      sync.RWMutex
}

// NewSchemaMapResolver creates a new SchemaMapResolver from a map of GVK to
// spec.Schema.
func NewSchemaMapResolver(schemas map[schema.GroupVersionKind]*spec.Schema) *SchemaMapResolver {
	return &SchemaMapResolver{schemas: schemas}
}

// ResolveSchema returns the OpenAPI schema for the given GVK.
func (r *SchemaMapResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	r.mx.RLock()
	defer r.mx.RUnlock()
	return r.schemas[gvk], nil
}
