// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package es

import (
	"testing"

	estype "github.com/elastic/cloud-on-k8s/pkg/apis/elasticsearch/v1alpha1"
	"github.com/elastic/cloud-on-k8s/pkg/utils/k8s"
	"github.com/elastic/cloud-on-k8s/test/e2e/test"
	"github.com/elastic/cloud-on-k8s/test/e2e/test/elasticsearch"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpdateESSecureSettings(t *testing.T) {
	k := test.NewK8sClientOrFatal()

	// user-provided secure settings secret

	const securePasswordSettingKey = "xpack.notification.email.account.foo.smtp.secure_password"
	const secureBarUserSettingKey = "xpack.notification.jira.account.bar.secure_user"
	const secureBazUserSettingKey = "xpack.notification.jira.account.baz.secure_user"
	secureSettings1 := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-secrets-1",
			Namespace: test.Ctx().ManagedNamespace(0),
		},
		Data: map[string][]byte{
			// this needs to be a valid configuration item, otherwise ES refuses to start
			securePasswordSettingKey: []byte("foo_pw"),
		},
	}
	secureSettings2 := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-secrets-2",
			Namespace: test.Ctx().ManagedNamespace(0),
		},
		Data: map[string][]byte{
			// this needs to be a valid configuration item, otherwise ES refuses to start
			secureBarUserSettingKey: []byte("bar_user"),
		},
	}

	secureSettings := []corev1.Secret{secureSettings1, secureSettings2}

	// set up a 3-nodes cluster with secure settings
	b := elasticsearch.NewBuilder("test-es-keystore").
		WithESMasterDataNodes(3, elasticsearch.DefaultResources).
		WithESSecureSettings(secureSettings1.Name, secureSettings2.Name)

	test.StepList{}.
		// create secure settings secret
		WithStep(test.Step{
			Name: "Create secure settings secret",
			Test: func(t *testing.T) {
				for _, s := range secureSettings {
					// remove if already exists (ignoring errors)
					_ = k.Client.Delete(&s)
					// and create a fresh one
					err := k.Client.Create(&s)
					require.NoError(t, err)

				}
			},
		}).

		// create the cluster
		WithSteps(b.InitTestSteps(k)).
		WithSteps(b.CreationTestSteps(k)).
		WithSteps(test.CheckTestSteps(b, k)).
		WithSteps(test.StepList{
			// initial secure settings should be there in all nodes keystore
			elasticsearch.CheckESKeystoreEntries(k, b.Elasticsearch, []string{
				securePasswordSettingKey,
				secureBarUserSettingKey}),

			// modify the secure settings secret
			test.Step{
				Name: "Modify secure settings secret",
				Test: func(t *testing.T) {
					// remove some keys, add new ones
					secureSettings2.Data = map[string][]byte{
						secureBazUserSettingKey: []byte("baz"), // the actual value update cannot be checked :(
					}
					err := k.Client.Update(&secureSettings2)
					require.NoError(t, err)
				},
			},

			// keystore should be updated accordingly
			elasticsearch.CheckESKeystoreEntries(k, b.Elasticsearch, []string{
				securePasswordSettingKey,
				secureBazUserSettingKey,
			}),
			test.Step{
				Name: "Remove one of the source secrets",
				Test: func(t *testing.T) {
					require.NoError(t, k.Client.Delete(&secureSettings2))
				},
			},
			elasticsearch.CheckESKeystoreEntries(k, b.Elasticsearch, []string{
				securePasswordSettingKey,
			}),

			// remove the secure settings reference
			test.Step{
				Name: "Remove secure settings from the spec",
				Test: func(t *testing.T) {
					// retrieve current Elasticsearch resource
					var currentEs estype.Elasticsearch
					err := k.Client.Get(k8s.ExtractNamespacedName(&b.Elasticsearch), &currentEs)
					require.NoError(t, err)
					// set its secure settings to nil
					currentEs.Spec.SecureSettings = nil
					err = k.Client.Update(&currentEs)
					require.NoError(t, err)
				},
			},

			// keystore should be updated accordingly
			elasticsearch.CheckESKeystoreEntries(k, b.Elasticsearch, nil),

			// cleanup extra resources
			test.Step{
				Name: "Delete secure settings secret",
				Test: func(t *testing.T) {
					err := k.Client.Delete(&secureSettings1) // we deleted the other one above already
					require.NoError(t, err)
				},
			},
		}).
		WithSteps(b.DeletionTestSteps(k)).
		RunSequential(t)
}
