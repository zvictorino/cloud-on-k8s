// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package runner

import (
	"fmt"
	"log"
)

func init() {
	drivers[AksDriverID] = &AksDriverFactory{}
}

const (
	AksDriverID                    = "aks"
	AksVaultPath                   = "secret/devops-ci/cloud-on-k8s/ci-azr-k8s-operator"
	AksResourceGroupVaultFieldName = "resource-group"
	AksAcrNameVaultFieldName       = "acr-name"
)

type AksDriverFactory struct {
}

type AksDriver struct {
	plan        Plan
	ctx         map[string]interface{}
	vaultClient *VaultClient
}

func (gdf *AksDriverFactory) Create(plan Plan) (Driver, error) {
	vaultClient, err := NewClient(*plan.VaultInfo)
	if err != nil {
		return nil, err
	}

	if plan.Aks.ResourceGroup == "" {
		resourceGroup, err := vaultClient.Get(AksVaultPath, AksResourceGroupVaultFieldName)
		if err != nil {
			return nil, err
		}
		plan.Aks.ResourceGroup = resourceGroup
	}

	if plan.Aks.AcrName == "" {
		acrName, err := vaultClient.Get(AksVaultPath, AksAcrNameVaultFieldName)
		if err != nil {
			return nil, err
		}
		plan.Aks.AcrName = acrName
	}

	return &AksDriver{
		plan: plan,
		ctx: map[string]interface{}{
			"ResourceGroup":     plan.Aks.ResourceGroup,
			"ClusterName":       plan.ClusterName,
			"NodeCount":         plan.Aks.NodeCount,
			"MachineType":       plan.MachineType,
			"KubernetesVersion": plan.KubernetesVersion,
			"AcrName":           plan.Aks.AcrName,
		},
		vaultClient: vaultClient,
	}, nil
}

func (d *AksDriver) Execute() error {
	if err := d.auth(); err != nil {
		return err
	}

	exists, err := d.clusterExists()
	if err != nil {
		return err
	}

	switch d.plan.Operation {
	case "delete":
		if exists {
			if err := d.delete(); err != nil {
				return err
			}
		} else {
			log.Printf("not deleting as cluster doesn't exist")
		}
	case "create":
		if exists {
			log.Printf("not creating as cluster exists")
		} else {
			if err := d.create(); err != nil {
				return err
			}

			if err := d.configureDocker(); err != nil {
				return err
			}
		}

		if err := d.GetCredentials(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown operation %s", d.plan.Operation)
	}

	return nil
}

func (d *AksDriver) auth() error {
	if d.plan.ServiceAccount {
		log.Print("Authenticating as service account...")

		secrets, err := d.vaultClient.GetMany(AksVaultPath, "appId", "password", "tenant")
		if err != nil {
			return err
		}
		appID, tenantSecret, tenantID := secrets[0], secrets[1], secrets[2]

		cmd := "az login --service-principal -u {{.AppId}} -p {{.TenantSecret}} --tenant {{.TenantId}}"
		return NewCommand(cmd).
			AsTemplate(map[string]interface{}{
				"AppId":        appID,
				"TenantSecret": tenantSecret,
				"TenantId":     tenantID,
			}).
			WithoutStreaming().
			Run()
	}

	log.Print("Authenticating as user...")
	return NewCommand("az login").Run()
}

func (d *AksDriver) clusterExists() (bool, error) {
	log.Print("Checking if cluster exists...")

	cmd := "az aks show --name {{.ClusterName}} --resource-group {{.ResourceGroup}}"
	contains, err := NewCommand(cmd).AsTemplate(d.ctx).WithoutStreaming().OutputContainsAny("not be found", "was not found")
	if contains {
		return false, nil
	}

	return err == nil, err
}

func (d *AksDriver) create() error {
	log.Print("Creating cluster...")

	servicePrincipal := ""
	if d.plan.ServiceAccount {
		// our service principal doesn't have permissions to create a service principal for aks cluster
		// instead, we reuse the current service principal as the one for aks cluster
		secrets, err := d.vaultClient.GetMany(AksVaultPath, "appId", "password")
		if err != nil {
			return err
		}
		servicePrincipal = fmt.Sprintf(" --service-principal %s --client-secret %s", secrets[0], secrets[1])
	}

	cmd := `az aks create --resource-group {{.ResourceGroup}} --name {{.ClusterName}} ` +
		`--node-count {{.NodeCount}} --node-vm-size {{.MachineType}} --kubernetes-version {{.KubernetesVersion}} ` +
		`--node-osdisk-size 30 --enable-addons http_application_routing,monitoring --generate-ssh-keys` + servicePrincipal
	if err := NewCommand(cmd).AsTemplate(d.ctx).Run(); err != nil {
		return err
	}

	return nil
}

func (d *AksDriver) configureDocker() error {
	log.Print("Configuring Docker...")
	if err := NewCommand("az acr login --name {{.AcrName}}").AsTemplate(d.ctx).Run(); err != nil {
		return err
	}

	if d.plan.ServiceAccount {
		// it's already set for the ServiceAccount
		return nil
	}

	cmd := `az aks show --resource-group {{.ResourceGroup}} --name {{.ClusterName}} --query "servicePrincipalProfile.clientId" --output tsv`
	clientIds, err := NewCommand(cmd).AsTemplate(d.ctx).StdoutOnly().OutputList()
	if err != nil {
		return err
	}

	cmd = `az acr show --resource-group {{.ResourceGroup}} --name {{.AcrName}} --query "id" --output tsv`
	acrIds, err := NewCommand(cmd).AsTemplate(d.ctx).StdoutOnly().OutputList()
	if err != nil {
		return err
	}

	return NewCommand(`az role assignment create --assignee {{.ClientId}} --role acrpull --scope {{.AcrId}}`).
		AsTemplate(map[string]interface{}{
			"ClientId": clientIds[0],
			"AcrId":    acrIds[0],
		}).
		Run()
}

func (d *AksDriver) GetCredentials() error {
	log.Print("Getting credentials...")
	cmd := `az aks get-credentials --resource-group {{.ResourceGroup}} --name {{.ClusterName}}`
	return NewCommand(cmd).AsTemplate(d.ctx).Run()
}

func (d *AksDriver) delete() error {
	log.Print("Deleting cluster...")
	cmd := "az aks delete --yes --name {{.ClusterName}} --resource-group {{.ResourceGroup}}"
	return NewCommand(cmd).AsTemplate(d.ctx).Run()
}
