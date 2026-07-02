/**
# Copyright (c) NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package state

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
)

const (
	// gfdImageEnvName is the fallback env var for the GFD image when the CR does not
	// specify repository/image/version.
	gfdImageEnvName = "GFD_IMAGE"
)

func NewStateGFD(
	k8sClient client.Client,
	namespace string,
	scheme *runtime.Scheme,
	manifestDir string) (State, error) {

	skel, err := newStateSkel(k8sClient, namespace, scheme, manifestDir,
		"state-gfd", "NVIDIA GPU Feature Discovery deployed in the cluster")
	if err != nil {
		return nil, err
	}
	return &configurableState{
		stateSkel: skel,
		isEnabled: func(cr *nvidiav1alpha1.GPUCluster) bool {
			return cr.Spec.GFD != nil && cr.Spec.GFD.IsEnabled()
		},
		imageOverride: func(cr *nvidiav1alpha1.GPUCluster) (string, string, string) {
			spec := cr.Spec.GFD
			return spec.Repository, spec.Image, spec.Version
		},
		imageEnvName:    gfdImageEnvName,
		buildRenderData: buildGFDRenderData,
	}, nil
}

func buildGFDRenderData(_ context.Context, s *configurableState, cr *nvidiav1alpha1.GPUCluster, imagePath, apiVersion string) (interface{}, error) {
	daemonsets := cr.Spec.Daemonsets
	return &gfdRenderData{
		GFD:                     &gfdSpec{Spec: cr.Spec.GFD, ImagePath: imagePath},
		Daemonsets:              &daemonsets,
		Namespace:               s.namespace,
		ResourceClaimAPIVersion: apiVersion,
	}, nil
}
