// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package runner

import (
	"fmt"
	"log"
	"os"
	"strings"
)

const (
	GkeDriverID                     = "gke"
	GkeVaultPath                    = "secret/devops-ci/cloud-on-k8s/ci-gcp-k8s-operator"
	GkeServiceAccountVaultFieldName = "service-account"
)

func init() {
	drivers[GkeDriverID] = &GkeDriverFactory{}
}

type GkeDriverFactory struct {
}

type GkeDriver struct {
	plan Plan
	ctx  map[string]interface{}
}

func (gdf *GkeDriverFactory) Create(plan Plan) (Driver, error) {
	return &GkeDriver{
		plan: plan,
		ctx: map[string]interface{}{
			"GCloudProject":     plan.Gke.GCloudProject,
			"ClusterName":       plan.ClusterName,
			"Region":            plan.Gke.Region,
			"AdminUsername":     plan.Gke.AdminUsername,
			"KubernetesVersion": plan.KubernetesVersion,
			"MachineType":       plan.MachineType,
			"LocalSsdCount":     plan.Gke.LocalSsdCount,
			"GcpScopes":         plan.Gke.GcpScopes,
			"NodeCountPerZone":  plan.Gke.NodeCountPerZone,
		},
	}, nil
}

func (d *GkeDriver) Execute() error {
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
			err = d.delete()
		} else {
			log.Printf("not deleting as cluster doesn't exist")
		}
	case "create":
		if exists {
			log.Printf("not creating as cluster exists")
		} else {
			if err := d.configSSH(); err != nil {
				return err
			}

			if err := d.create(); err != nil {
				return err
			}

			if err := d.bindRoles(); err != nil {
				return err
			}
		}

		if err := d.GetCredentials(); err != nil {
			return err
		}

		if d.plan.VmMapMax {
			if err := d.setMaxMapCount(); err != nil {
				return err
			}
		}

		if err := d.configureDocker(); err != nil {
			return err
		}

		if err := d.patchStorageClass(); err != nil {
			return err
		}
	default:
		err = fmt.Errorf("unknown operation %s", d.plan.Operation)
	}

	return err
}

func (d *GkeDriver) auth() error {
	if d.plan.ServiceAccount {
		log.Println("Authenticating as service account...")

		client, err := NewClient(*d.plan.VaultInfo)
		if err != nil {
			return err
		}

		keyFileName := "gke_service_account_key.json"
		defer os.Remove(keyFileName)
		if err := client.ReadIntoFile(keyFileName, GkeVaultPath, GkeServiceAccountVaultFieldName); err != nil {
			return err
		}

		return NewCommand("gcloud auth activate-service-account --key-file=" + keyFileName).Run()
	}

	log.Println("Authenticating as user...")
	accounts, err := NewCommand(`gcloud auth list "--format=value(account)"`).StdoutOnly().WithoutStreaming().Output()
	if err != nil {
		return err
	}

	if len(accounts) > 0 {
		return nil
	}

	return NewCommand("gcloud auth login").Run()
}

func (d *GkeDriver) clusterExists() (bool, error) {
	log.Println("Checking if cluster exists...")

	cmd := "gcloud beta container clusters --project {{.GCloudProject}} describe {{.ClusterName}} --region {{.Region}}"
	contains, err := NewCommand(cmd).AsTemplate(d.ctx).WithoutStreaming().OutputContainsAny("Not found")
	if contains {
		return false, nil
	}

	return err == nil, err
}

func (d *GkeDriver) configSSH() error {
	log.Println("Configuring ssh...")
	return NewCommand("gcloud --quiet --project {{.GCloudProject}} compute config-ssh").AsTemplate(d.ctx).Run()
}

func (d *GkeDriver) create() error {
	log.Println("Creating cluster...")
	pspOption := ""
	if d.plan.Psp {
		pspOption = " --enable-pod-security-policy"
	}

	return NewCommand(`gcloud beta container --project {{.GCloudProject}} clusters create {{.ClusterName}} ` +
		`--region {{.Region}} --username {{.AdminUsername}} --cluster-version {{.KubernetesVersion}} ` +
		`--machine-type {{.MachineType}} --image-type COS --disk-type pd-ssd --disk-size 30 ` +
		`--local-ssd-count {{.LocalSsdCount}} --scopes {{.GcpScopes}} --num-nodes {{.NodeCountPerZone}} ` +
		`--enable-cloud-logging --enable-cloud-monitoring --addons HorizontalPodAutoscaling,HttpLoadBalancing ` +
		`--no-enable-autoupgrade --no-enable-autorepair --network projects/{{.GCloudProject}}/global/networks/default ` +
		`--subnetwork projects/{{.GCloudProject}}/regions/{{.Region}}/subnetworks/default` + pspOption).
		AsTemplate(d.ctx).
		Run()
}

func (d *GkeDriver) bindRoles() error {
	log.Println("Binding roles...")
	user, err := NewCommand(`gcloud auth list --filter=status:ACTIVE --format="value(account)"`).Output()
	if err != nil {
		return err
	}
	cmd := "kubectl create clusterrolebinding cluster-admin-binding --clusterrole=cluster-admin --user=" + user
	return NewCommand(cmd).Run()
}

func (d *GkeDriver) setMaxMapCount() error {
	log.Println("Setting max map count...")
	instances, err := NewCommand(`gcloud compute instances list --project={{.GCloudProject}} ` +
		`--filter="metadata.items.key['cluster-name']['value']='{{.ClusterName}}' AND metadata.items.key['cluster-name']['value']!='' " ` +
		`--format="value[separator=','](name,zone)"`).
		AsTemplate(d.ctx).
		StdoutOnly().
		OutputList()
	if err != nil {
		return err
	}

	for _, instance := range instances {
		nameZone := strings.Split(instance, ",")
		if len(nameZone) != 2 {
			return fmt.Errorf("instance %s could not be parsed", instance)
		}

		name, zone := nameZone[0], nameZone[1]
		if err := NewCommand(`gcloud -q compute ssh jenkins@{{.Name}} --project={{.GCloudProject}} --zone={{.Zone}} --command="sudo sysctl -w vm.max_map_count=262144"`).
			AsTemplate(map[string]interface{}{
				"GCloudProject": d.plan.Gke.GCloudProject,
				"Name":          name,
				"Zone":          zone,
			}).
			Run(); err != nil {
			return err
		}
	}

	return nil
}

func (d *GkeDriver) configureDocker() error {
	log.Println("Configuring Docker...")
	return NewCommand("gcloud auth configure-docker --quiet").Run()
}

func (d *GkeDriver) GetCredentials() error {
	log.Println("Getting credentials...")
	cmd := "gcloud container clusters --project {{.GCloudProject}} get-credentials {{.ClusterName}} --region {{.Region}}"
	return NewCommand(cmd).AsTemplate(d.ctx).Run()
}

// patchStorageClass based on standard storageclass, creates new default with "volumeBindingMode: WaitForFirstConsumer"
func (d *GkeDriver) patchStorageClass() error {
	log.Println("Patching storage class...")

	if exists, err := NewCommand("kubectl get sc").OutputContainsAny("standard-customized"); err != nil {
		return err
	} else if exists {
		return nil
	}

	defaultName := ""
	for _, annotation := range []string{
		`storageclass\.kubernetes\.io/is-default-class`,
		`storageclass\.beta\.kubernetes\.io/is-default-class`,
	} {
		template := `kubectl get sc -o=jsonpath="{$.items[?(@.metadata.annotations.%s=='true')].metadata.name}"`
		baseScs, err := NewCommand(fmt.Sprintf(template, annotation)).OutputList()
		if err != nil {
			return err
		}

		if len(baseScs) != 0 {
			defaultName = baseScs[0]
			break
		}
	}

	if defaultName == "" {
		return fmt.Errorf("default storageclass not found")
	}

	sc, err := NewCommand(fmt.Sprintf("kubectl get sc %s -o yaml", defaultName)).Output()
	if err != nil {
		return err
	}

	sc = strings.Replace(sc, fmt.Sprintf("name: %s", defaultName), "name: standard-customized", -1)
	sc = strings.Replace(sc, "volumeBindingMode: Immediate", "volumeBindingMode: WaitForFirstConsumer", -1)
	err = NewCommand(fmt.Sprintf(`cat <<EOF | kubectl apply -f -
%s
EOF`, sc)).Run()
	if err != nil {
		return err
	}

	cmd := fmt.Sprintf(
		`kubectl patch storageclass %s -p '{ "metadata": { "annotations": { "storageclass.beta.kubernetes.io/is-default-class":"false"} } }'`,
		defaultName)
	return NewCommand(cmd).Run()
}

func (d *GkeDriver) delete() error {
	log.Println("Deleting cluster...")
	cmd := "gcloud beta --quiet --project {{.GCloudProject}} container clusters delete {{.ClusterName}} --region {{.Region}}"
	if err := NewCommand(cmd).AsTemplate(d.ctx).Run(); err != nil {
		return err
	}

	// Deleting clusters in GKE does not delete associated disks, we have to delete them manually.
	cmd = `gcloud compute disks list --filter="-users:*" --format="value[separator=','](name,zone)" --project {{.GCloudProject}}`
	disks, err := NewCommand(cmd).AsTemplate(d.ctx).StdoutOnly().OutputList()
	if err != nil {
		return err
	}

	for _, disk := range disks {
		nameZone := strings.Split(disk, ",")
		if len(nameZone) != 2 {
			return fmt.Errorf("disk name and zone contained unexpected number of fields")
		}

		name, zone := nameZone[0], nameZone[1]
		cmd = `gcloud compute disks delete {{.Name}} --project {{.GCloudProject}} --zone {{.Zone}} --quiet`
		err := NewCommand(cmd).
			AsTemplate(map[string]interface{}{
				"GCloudProject": d.plan.Gke.GCloudProject,
				"Name":          name,
				"Zone":          zone,
			}).
			Run()
		if err != nil {
			return err
		}
	}

	return nil
}
