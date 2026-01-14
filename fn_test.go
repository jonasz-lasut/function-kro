package main

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/crossplane/crossplane-runtime/pkg/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

func TestRunFunction(t *testing.T) {
	type args struct {
		ctx context.Context
		req *fnv1.RunFunctionRequest
	}
	type want struct {
		rsp *fnv1.RunFunctionResponse
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"MissingCRDs": {
			reason: "The function should return requirements when CRDs are not yet available",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"spec": {
									"forProvider": {
										"region": "us-west-2"
									}
								}
							}
						}],
						"status": {
							"bucketName": "${bucket.status.atProvider.id}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {}
							}`),
						},
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Requirements: &fnv1.Requirements{
						ExtraResources: map[string]*fnv1.ResourceSelector{
							"example.crossplane.io/v1, Kind=XBucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "xbuckets.example.crossplane.io"},
							},
							"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "buckets.s3.aws.upbound.io"},
							},
						},
					},
				},
			},
		},
		"DesiredXROnlyContainsDeclaredStatus": {
			reason: "The desired XR should only contain status fields declared in the ResourceGraph, not the full observed XR",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"spec": {
									"forProvider": {
										"region": "us-west-2"
									}
								}
							}
						}],
						"status": {
							"bucketName": "${bucket.status.atProvider.id}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {
									"name": "test-bucket",
									"uid": "abc-123",
									"resourceVersion": "12345",
									"generation": 2
								},
								"spec": {"bucketName": "my-bucket"},
								"status": {
									"conditions": [{"type": "Ready", "status": "True"}]
								}
							}`),
						},
					},
					ExtraResources: map[string]*fnv1.Resources{
						"example.crossplane.io/v1, Kind=XBucket": {
							Items: []*fnv1.Resource{{
								Resource: resource.MustStructJSON(`{
									"apiVersion": "apiextensions.k8s.io/v1",
									"kind": "CustomResourceDefinition",
									"metadata": {"name": "xbuckets.example.crossplane.io"},
									"spec": {
										"group": "example.crossplane.io",
										"names": {"kind": "XBucket", "plural": "xbuckets"},
										"scope": "Cluster",
										"versions": [{
											"name": "v1",
											"served": true,
											"storage": true,
											"schema": {
												"openAPIV3Schema": {
													"type": "object",
													"properties": {
														"apiVersion": {"type": "string"},
														"kind": {"type": "string"},
														"metadata": {"type": "object"},
														"spec": {
															"type": "object",
															"properties": {
																"bucketName": {"type": "string"}
															}
														},
														"status": {
															"type": "object",
															"properties": {
																"bucketName": {"type": "string"}
															}
														}
													}
												}
											}
										}]
									}
								}`),
							}},
						},
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
							Items: []*fnv1.Resource{{
								Resource: resource.MustStructJSON(`{
									"apiVersion": "apiextensions.k8s.io/v1",
									"kind": "CustomResourceDefinition",
									"metadata": {"name": "buckets.s3.aws.upbound.io"},
									"spec": {
										"group": "s3.aws.upbound.io",
										"names": {"kind": "Bucket", "plural": "buckets"},
										"scope": "Cluster",
										"versions": [{
											"name": "v1beta1",
											"served": true,
											"storage": true,
											"schema": {
												"openAPIV3Schema": {
													"type": "object",
													"properties": {
														"apiVersion": {"type": "string"},
														"kind": {"type": "string"},
														"metadata": {"type": "object"},
														"spec": {
															"type": "object",
															"properties": {
																"forProvider": {
																	"type": "object",
																	"properties": {
																		"region": {"type": "string"}
																	}
																}
															}
														},
														"status": {
															"type": "object",
															"properties": {
																"atProvider": {
																	"type": "object",
																	"properties": {
																		"id": {"type": "string"}
																	}
																}
															}
														}
													}
												}
											}
										}]
									}
								}`),
							}},
						},
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Requirements: &fnv1.Requirements{
						ExtraResources: map[string]*fnv1.ResourceSelector{
							"example.crossplane.io/v1, Kind=XBucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "xbuckets.example.crossplane.io"},
							},
							"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "buckets.s3.aws.upbound.io"},
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket"
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									}
								}`),
								Ready: fnv1.Ready_READY_FALSE,
							},
						},
					},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &Function{log: logging.NewNopLogger()}
			rsp, err := f.RunFunction(tc.args.ctx, tc.args.req)

			if diff := cmp.Diff(tc.want.rsp, rsp, protocmp.Transform()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want rsp, +got rsp:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.err, err, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want err, +got err:\n%s", tc.reason, diff)
			}
		})
	}
}
