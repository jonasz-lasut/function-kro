package main

import (
	"context"
	"strings"

	"github.com/upbound/function-kro/input/v1beta1"
	"github.com/upbound/function-kro/kro/graph"
	kroschema "github.com/upbound/function-kro/kro/graph/schema"
	schemaresolver "github.com/upbound/function-kro/kro/graph/schema/resolver"
	"github.com/upbound/function-kro/kro/runtime"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/fieldpath"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/resource/composed"
	"github.com/crossplane/function-sdk-go/resource/composite"
	"github.com/crossplane/function-sdk-go/response"
)

// Function returns whatever response you ask it to.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log logging.Logger
}

// RunFunction runs the Function.
func (f *Function) RunFunction(_ context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running function", "tag", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	rg := &v1beta1.ResourceGraph{}
	if err := request.GetInput(req, rg); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composite resource"))
		return rsp, nil
	}

	// Collect all GVKs we need schemas for: the XR and all resource templates.
	gvks := make([]schema.GroupVersionKind, 0, len(rg.Resources)+1)
	xrGVK := schema.FromAPIVersionAndKind(oxr.Resource.GetAPIVersion(), oxr.Resource.GetKind())
	gvks = append(gvks, xrGVK)
	for _, r := range rg.Resources {
		// Skip ExternalRef resources - they reference existing resources, not templates
		if r.ExternalRef != nil {
			gvks = append(gvks, schema.FromAPIVersionAndKind(r.ExternalRef.APIVersion, r.ExternalRef.Kind))
			continue
		}
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(r.Template.Raw, u); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot unmarshal resource id %q", r.ID))
			return rsp, nil
		}
		gvks = append(gvks, schema.FromAPIVersionAndKind(u.GetAPIVersion(), u.GetKind()))
	}

	// Request BOTH schemas (new path) AND CRDs (fallback path).
	// This allows the function to work with both newer Crossplane versions
	// that support required_schemas and older versions that only support required_resources.
	rsp.Requirements = &fnv1.Requirements{
		Schemas:   RequiredSchemas(gvks...).Schemas,
		Resources: RequiredCRDs(gvks...).Resources,
	}

	// Try the required_schemas path first (preferred).
	combinedResolver, xrSchema, err := f.trySchemaPath(req, gvks, xrGVK)
	if err != nil {
		response.Fatal(rsp, err)
		return rsp, nil
	}

	// If schemas weren't available, fall back to CRD extraction.
	if combinedResolver == nil {
		combinedResolver, xrSchema, err = f.tryCRDPath(req, gvks, xrGVK)
		if err != nil {
			response.Fatal(rsp, err)
			return rsp, nil
		}
	}

	// If neither path succeeded, we're still waiting for Crossplane to send us resources.
	if combinedResolver == nil {
		f.log.Debug("Waiting for Crossplane to provide schemas or CRDs")
		return rsp, nil
	}

	// Build the graph using the resolver.
	gb := graph.NewBuilder(combinedResolver, nil)

	g, err := gb.NewResourceGraphDefinition(rg, xrSchema)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create resource graph"))
		return rsp, nil
	}

	rt, err := runtime.FromGraph(g, &oxr.Resource.Unstructured)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create graph runtime"))
		return rsp, nil
	}

	ocds, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composed resources"))
		return rsp, nil
	}

	// Build a map from node ID to node for fast lookups.
	nodesByID := make(map[string]*runtime.Node)
	for _, node := range rt.Nodes() {
		nodesByID[node.Spec.Meta.ID] = node
	}

	// Set observed state on each node from observed composed resources.
	// For single resources, the composed resource name equals the node ID.
	// For collections, multiple resources map to the same node ID (handled later).
	ready := make(map[string]bool)
	for name, r := range ocds {
		id := string(name)
		node, ok := nodesByID[id]
		if !ok {
			// This resource doesn't match any node - might be from a different function
			// or a stale resource. Skip it.
			f.log.Debug("Observed resource has no matching node", "name", name)
			continue
		}

		node.SetObserved([]*unstructured.Unstructured{&r.Resource.Unstructured})

		isReady, err := node.IsReady()
		if err != nil {
			f.log.Info("Error checking resource readiness", "id", id, "err", err)
			continue
		}
		if !isReady {
			f.log.Debug("Resource isn't ready yet", "id", id)
			continue
		}

		ready[id] = true
	}

	dcds, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired composed resources"))
		return rsp, nil
	}

	// Process nodes in topological order, generating desired state.
	for _, node := range rt.Nodes() {
		id := node.Spec.Meta.ID

		// Check if this node should be ignored (includeWhen evaluated to false).
		ignored, err := node.IsIgnored()
		if err != nil {
			f.log.Info("Error checking if resource is ignored", "id", id, "err", err)
			continue
		}
		if ignored {
			f.log.Debug("Skipping ignored resource", "id", id)
			continue
		}

		// Get the desired state with CEL expressions resolved.
		// This is critical for SSA - desired state must only contain fields
		// we want to own, not provider-defaulted fields from observed state.
		desired, err := node.GetDesired()
		if err != nil {
			if runtime.IsDataPending(err) {
				f.log.Debug("Skipping resource with pending data", "id", id)
				continue
			}
			response.Fatal(rsp, errors.Wrapf(err, "cannot get desired state for resource %q", id))
			return rsp, nil
		}

		// For single resources, desired has one element.
		// For collections, desired has multiple elements (one per forEach expansion).
		for i, r := range desired {
			resourceName := id
			if len(desired) > 1 {
				// Collection: append index to make unique resource names
				resourceName = id + "-" + string(rune('0'+i))
			}

			cd, err := composed.From(r)
			if err != nil {
				response.Fatal(rsp, errors.Wrapf(err, "cannot create composed resource from template id %s", id))
				return rsp, nil
			}
			dcds[resource.Name(resourceName)] = &resource.DesiredComposed{Resource: cd, Ready: resource.ReadyFalse}
			if ready[id] {
				dcds[resource.Name(resourceName)].Ready = resource.ReadyTrue
			}
		}
	}

	if err := response.SetDesiredComposedResources(rsp, dcds); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composed resources"))
		return rsp, nil
	}

	// Build a minimal desired XR containing only the status paths declared in
	// the ResourceGraph. This is critical for SSA - we must only include fields
	// we want to own. The runtime uses soft resolution for instance status,
	// returning only fields where all CEL expressions were successfully resolved.
	dxr := &composite.Unstructured{Unstructured: unstructured.Unstructured{Object: map[string]any{}}}
	dxr.SetAPIVersion(oxr.Resource.GetAPIVersion())
	dxr.SetKind(oxr.Resource.GetKind())

	// Get the resolved status fields from the instance node.
	// GetDesired on the instance node uses soft resolution - it returns partial
	// results when some expressions can't be evaluated yet.
	instanceDesired, err := rt.Instance().GetDesired()
	if err != nil {
		// Errors from instance GetDesired are unexpected since it uses soft resolution.
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired instance status"))
		return rsp, nil
	}

	// Copy resolved status fields to the desired XR.
	if len(instanceDesired) > 0 && instanceDesired[0] != nil {
		src := fieldpath.Pave(instanceDesired[0].Object)
		dst := fieldpath.Pave(dxr.Object)
		for _, v := range g.Instance.Variables {
			val, err := src.GetValue(v.Path)
			if err != nil {
				// Value not resolved yet (CEL dependency not satisfied), skip it.
				continue
			}
			if err := dst.SetValue(v.Path, val); err != nil {
				response.Fatal(rsp, errors.Wrapf(err, "cannot set desired XR status field %q", v.Path))
				return rsp, nil
			}
		}
	}

	if err := response.SetDesiredCompositeResource(rsp, &resource.Composite{Resource: dxr}); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	return rsp, nil
}

// trySchemaPath attempts to build a resolver from Crossplane's required_schemas.
// Returns (nil, nil, nil) if schemas aren't available yet (not an error).
// Returns (resolver, xrSchema, nil) on success.
// Returns (nil, nil, error) on fatal errors.
func (f *Function) trySchemaPath(req *fnv1.RunFunctionRequest, gvks []schema.GroupVersionKind, xrGVK schema.GroupVersionKind) (resolver.SchemaResolver, *spec.Schema, error) {
	reqSchemas := req.GetRequiredSchemas()
	if len(reqSchemas) == 0 {
		// Crossplane hasn't sent any schemas - either it doesn't support
		// required_schemas or hasn't processed our requirements yet.
		return nil, nil, nil
	}

	schemas := make(map[schema.GroupVersionKind]*spec.Schema)
	for _, gvk := range gvks {
		s, ok := reqSchemas[gvk.String()]
		if !ok {
			// This GVK wasn't in the response. Could be a built-in type
			// that Crossplane doesn't have a schema for.
			f.log.Debug("Schema not in required_schemas response", "gvk", gvk.String())
			continue
		}

		if s.GetOpenapiV3() == nil {
			// Schema exists but has no content - might be built-in type.
			f.log.Debug("Schema has no OpenAPI v3 content", "gvk", gvk.String())
			continue
		}

		specSchema, err := schemaresolver.StructToSpecSchema(s.GetOpenapiV3())
		if err != nil {
			return nil, nil, errors.Wrapf(err, "cannot convert schema for %q", gvk)
		}

		// Inject full ObjectMeta schema since CRD schemas typically have
		// incomplete metadata definitions.
		if specSchema.Properties == nil {
			specSchema.Properties = make(map[string]spec.Schema)
		}
		specSchema.Properties["metadata"] = kroschema.ObjectMetaSchema

		schemas[gvk] = specSchema
	}

	// If we got no schemas at all from this path, return nil to try CRD path.
	if len(schemas) == 0 {
		return nil, nil, nil
	}

	// Create combined resolver: schema map + built-in core types.
	schemaMapResolver := schemaresolver.NewSchemaMapResolver(schemas)
	combinedResolver := schemaresolver.NewCombinedResolverFromSchemas(schemaMapResolver)

	// Get XR schema from the combined resolver.
	xrSchema, err := combinedResolver.ResolveSchema(xrGVK)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "cannot resolve schema for XR %q", xrGVK)
	}
	if xrSchema == nil {
		return nil, nil, errors.Errorf("schema for XR %q not found", xrGVK)
	}

	f.log.Debug("Using required_schemas path")
	return combinedResolver, xrSchema, nil
}

// tryCRDPath attempts to build a resolver by extracting schemas from CRDs.
// Returns (nil, nil, nil) if CRDs aren't available yet (not an error).
// Returns (resolver, xrSchema, nil) on success.
// Returns (nil, nil, error) on fatal errors.
func (f *Function) tryCRDPath(req *fnv1.RunFunctionRequest, gvks []schema.GroupVersionKind, xrGVK schema.GroupVersionKind) (resolver.SchemaResolver, *spec.Schema, error) {
	requiredResources := req.GetRequiredResources()
	if len(requiredResources) == 0 {
		// Crossplane hasn't sent any CRDs yet.
		return nil, nil, nil
	}

	crds := make([]*extv1.CustomResourceDefinition, 0, len(gvks))
	for _, gvk := range gvks {
		e, ok := requiredResources[gvk.String()]
		if !ok {
			// This GVK wasn't in the response - Crossplane might not have
			// processed our requirements yet.
			f.log.Debug("CRD not in required_resources response", "gvk", gvk.String())
			return nil, nil, nil
		}

		if len(e.GetItems()) < 1 {
			// Crossplane is telling us the CRD doesn't exist.
			// This might be a built-in type without a CRD.
			f.log.Debug("CRD unavailable", "gvk", gvk.String())
			continue
		}

		crd := &extv1.CustomResourceDefinition{}
		if err := resource.AsObject(e.GetItems()[0].GetResource(), crd); err != nil {
			return nil, nil, errors.Wrapf(err, "cannot unmarshal CRD for %q", gvk)
		}

		crds = append(crds, crd)
	}

	if len(crds) == 0 {
		// No CRDs available yet.
		return nil, nil, nil
	}

	// Create combined resolver from CRDs.
	combinedResolver, err := schemaresolver.NewCombinedResolverFromCRDs(crds)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot create schema resolver from CRDs")
	}

	// Get XR schema from the combined resolver.
	xrSchema, err := combinedResolver.ResolveSchema(xrGVK)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "cannot resolve schema for XR %q", xrGVK)
	}
	if xrSchema == nil {
		return nil, nil, errors.Errorf("schema for XR %q not found", xrGVK)
	}

	f.log.Debug("Using required_resources CRD path")
	return combinedResolver, xrSchema, nil
}

// RequiredSchemas returns the schema requirements for the given GVKs.
// This tells Crossplane which OpenAPI schemas the function needs.
func RequiredSchemas(gvks ...schema.GroupVersionKind) *fnv1.Requirements {
	rq := &fnv1.Requirements{Schemas: map[string]*fnv1.SchemaSelector{}}

	for _, gvk := range gvks {
		rq.Schemas[gvk.String()] = &fnv1.SchemaSelector{
			ApiVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
		}
	}

	return rq
}

// RequiredCRDs returns the required CRDs this function requires to run.
// This is the fallback path for older Crossplane versions that don't support required_schemas.
func RequiredCRDs(gvks ...schema.GroupVersionKind) *fnv1.Requirements {
	rq := &fnv1.Requirements{Resources: map[string]*fnv1.ResourceSelector{}}

	for _, gvk := range gvks {
		rq.Resources[gvk.String()] = &fnv1.ResourceSelector{
			ApiVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
			Match: &fnv1.ResourceSelector_MatchName{
				MatchName: strings.ToLower(gvk.Kind + "s." + gvk.Group),
			},
		}
	}

	return rq
}
