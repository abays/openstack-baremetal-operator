/*

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

package openstackprovisionserver

import (
	"fmt"

	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	baremetalv1 "github.com/openstack-k8s-operators/openstack-baremetal-operator/api/v1beta1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// ServiceCommand -
	ServiceCommand = "cp -f /usr/local/apache2/conf/httpd.conf /etc/httpd/conf/httpd.conf && /usr/bin/run-httpd"
)

// Deployment func
func Deployment(
	instance *baremetalv1.OpenStackProvisionServer,
	configHash string,
	labels map[string]string,
	provInterfaceName string,
) *appsv1.Deployment {
	runAsUser := int64(0)

	livenessProbe := &corev1.Probe{
		// TODO might need tuning
		TimeoutSeconds:      5,
		PeriodSeconds:       3,
		InitialDelaySeconds: 3,
	}
	readinessProbe := &corev1.Probe{
		// TODO might need tuning
		TimeoutSeconds:      5,
		PeriodSeconds:       5,
		InitialDelaySeconds: 5,
	}

	args := []string{"-c"}
	args = append(args, ServiceCommand)
	//
	// https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/
	//

	port := instance.Spec.Port

	livenessProbe.HTTPGet = &corev1.HTTPGetAction{
		Path: "/",
		Port: intstr.IntOrString{Type: intstr.Int, IntVal: port},
	}
	readinessProbe.HTTPGet = &corev1.HTTPGetAction{
		Path: "/",
		Port: intstr.IntOrString{Type: intstr.Int, IntVal: port},
	}

	envVars := map[string]env.Setter{}
	envVars["CONFIG_HASH"] = env.SetValue(configHash)

	replicas := int32(1)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-openstackprovisionserver", instance.Name),
			Namespace: instance.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: ServiceAccount,
					HostNetwork:        true,
					Containers: []corev1.Container{
						{
							Name: "osp-httpd",
							Command: []string{
								"/bin/bash",
							},
							Args:  args,
							Image: instance.Spec.ApacheImageURL,
							SecurityContext: &corev1.SecurityContext{
								RunAsUser: &runAsUser,
							},
							Env:            env.MergeEnvs([]corev1.EnvVar{}, envVars),
							VolumeMounts:   getVolumeMounts(),
							Resources:      instance.Spec.Resources,
							ReadinessProbe: readinessProbe,
							LivenessProbe:  livenessProbe,
						},
						{
							Name:            "osp-provision-ip-discovery-agent",
							Command:         []string{"/osp-director-agent", "provision-ip-discovery"},
							Image:           instance.Spec.AgentImageURL,
							ImagePullPolicy: corev1.PullAlways,
							Env: []corev1.EnvVar{
								{
									Name:  "PROV_INTF",
									Value: provInterfaceName,
								},
								{
									Name:  "PROV_SERVER_NAME",
									Value: instance.GetName(),
								},
								{
									Name:  "PROV_SERVER_NAMESPACE",
									Value: instance.GetNamespace(),
								},
							},
						},
					},
				},
			},
		},
	}
	deployment.Spec.Template.Spec.Volumes = getVolumes()
	// Due to host networking, provision servers must run on separate worker nodes
	deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "app",
								Operator: metav1.LabelSelectorOperator(corev1.NodeSelectorOpIn),
								Values:   []string{AppLabel},
							},
						},
					},
					Namespaces:  []string{instance.Namespace},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		},
	}

	if instance.Spec.NodeSelector != nil && len(instance.Spec.NodeSelector) > 0 {
		deployment.Spec.Template.Spec.NodeSelector = instance.Spec.NodeSelector
	}

	initContainerDetails := InitContainerDetails{
		ContainerImage: instance.Spec.DownloaderImageURL,
		RhelImage:      instance.Spec.RhelImageURL,
		VolumeMounts:   getInitVolumeMounts(),
	}
	deployment.Spec.Template.Spec.InitContainers = InitContainer(initContainerDetails)

	return deployment
}