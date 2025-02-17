// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package sset

import (
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/elastic/cloud-on-k8s/pkg/apis/elasticsearch/v1alpha1"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/label"
	"github.com/elastic/cloud-on-k8s/pkg/utils/k8s"
	"github.com/elastic/cloud-on-k8s/pkg/utils/stringsutil"
)

// PodName returns the name of the pod with the given ordinal for this StatefulSet.
func PodName(ssetName string, ordinal int32) string {
	return fmt.Sprintf("%s-%d", ssetName, ordinal)
}

// PodNames returns the names of the pods for this StatefulSet, according to the number of replicas.
func PodNames(sset appsv1.StatefulSet) []string {
	names := make([]string, 0, GetReplicas(sset))
	for i := int32(0); i < GetReplicas(sset); i++ {
		names = append(names, PodName(sset.Name, i))
	}
	return names
}

// PodRevision returns the StatefulSet revision from this pod labels.
func PodRevision(pod corev1.Pod) string {
	return pod.Labels[appsv1.StatefulSetRevisionLabel]
}

// GetActualPodsForStatefulSet returns the existing pods associated to this StatefulSet.
// The returned pods may not match the expected StatefulSet replicas in a transient situation.
func GetActualPodsForStatefulSet(c k8s.Client, sset appsv1.StatefulSet) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := c.List(&client.ListOptions{
		Namespace: sset.Namespace,
		LabelSelector: labels.SelectorFromSet(map[string]string{
			label.StatefulSetNameLabelName: sset.Name,
		}),
	}, &pods); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// GetActualPodsForCluster return the existing pods associated to this cluster.
func GetActualPodsForCluster(c k8s.Client, es v1alpha1.Elasticsearch) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := c.List(&client.ListOptions{
		Namespace: es.Namespace,
		LabelSelector: labels.SelectorFromSet(map[string]string{
			label.ClusterNameLabelName: es.Name,
		}),
	}, &pods); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// GetActualMastersForCluster returns the list of existing master-eligible pods for the cluster.
func GetActualMastersForCluster(c k8s.Client, es v1alpha1.Elasticsearch) ([]corev1.Pod, error) {
	pods, err := GetActualPodsForCluster(c, es)
	if err != nil {
		return nil, err
	}
	return label.FilterMasterNodePods(pods), nil
}

func PodReconciliationDoneForSset(c k8s.Client, statefulSet appsv1.StatefulSet) (bool, error) {
	// check all expected pods are there: no more, no less
	actualPods, err := GetActualPodsForStatefulSet(c, statefulSet)
	if err != nil {
		return false, err
	}
	actualPodNames := k8s.PodNames(actualPods)
	expectedPodNames := PodNames(statefulSet)
	if !(len(actualPodNames) == len(expectedPodNames) && stringsutil.StringsInSlice(expectedPodNames, actualPodNames)) {
		log.V(1).Info(
			"Some pods still need to be created/deleted",
			"namespace", statefulSet.Namespace, "statefulset_name", statefulSet.Name,
			"expected_pods", expectedPodNames, "actual_pods", actualPodNames,
		)
		return false, nil
	}

	return true, nil
}

// StatefulSetName returns the name of the statefulset a Pod belongs to.
func StatefulSetName(podName string) (ssetName string, ordinal int32, err error) {
	ordinalPos := strings.LastIndex(podName, "-")
	ordinalAsString := podName[ordinalPos+1:]
	ordinalAsInt, err := strconv.Atoi(ordinalAsString)
	return podName[:ordinalPos], int32(ordinalAsInt), err
}
