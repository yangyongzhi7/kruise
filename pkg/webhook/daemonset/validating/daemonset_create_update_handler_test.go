/*
Copyright 2020 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validating

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
)

func newDaemonset(name string) *appsv1alpha1.DaemonSet {
	ds := &appsv1alpha1.DaemonSet{}
	ds.Name = name
	ds.Namespace = metav1.NamespaceDefault
	return ds
}

func TestValidateDaemonSet(t *testing.T) {
	a := assert.New(t)
	handler := DaemonSetCreateUpdateHandler{}

	for _, c := range []struct {
		Title             string
		Ds                *appsv1alpha1.DaemonSet
		ExpectAllowResult bool
	}{
		{
			"selector not match",
			func() *appsv1alpha1.DaemonSet {
				ds := newDaemonset("ds1")
				ds.Spec.Selector = &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"key1": "value1",
					},
				}
				ds.Spec.Template = corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"key1": "value2",
						},
					},
					Spec: corev1.PodSpec{},
				}
				return ds
			}(),
			false,
		},
		{
			"selector match",
			func() *appsv1alpha1.DaemonSet {
				ds := newDaemonset("ds1")
				ds.Spec.Selector = &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"key1": "value1",
					},
				}
				ds.Spec.Template = corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"key1": "value1",
						},
					},
					Spec: corev1.PodSpec{},
				}
				ds.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
				ds.Spec.UpdateStrategy = appsv1alpha1.DaemonSetUpdateStrategy{
					Type: appsv1alpha1.RollingUpdateDaemonSetStrategyType,
					RollingUpdate: &appsv1alpha1.RollingUpdateDaemonSet{
						Type: appsv1alpha1.StandardRollingUpdateType,
					},
				}
				return ds
			}(),
			true,
		},
	} {
		t.Logf("\t%s", c.Title)
		result, _, _ := handler.validatingDaemonSetFn(context.TODO(), c.Ds)
		a.Equal(c.ExpectAllowResult, result)
	}
}
