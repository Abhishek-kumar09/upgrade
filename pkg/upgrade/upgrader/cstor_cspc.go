/*
Copyright 2020 The OpenEBS Authors

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

package upgrader

import (
	"context"
	"time"

	cstor "github.com/openebs/api/v3/pkg/apis/cstor/v1"
	v1Alpha1API "github.com/openebs/api/v3/pkg/apis/openebs.io/v1alpha1"
	"github.com/openebs/upgrade/pkg/upgrade/patch"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

// CSPCPatch is the patch required to upgrade CSPC
type CSPCPatch struct {
	*ResourcePatch
	Namespace string
	CSPC      *patch.CSPC
	*Client
}

// CSPCPatchOptions ...
type CSPCPatchOptions func(*CSPCPatch)

// WithCSPCResorcePatch ...
func WithCSPCResorcePatch(r *ResourcePatch) CSPCPatchOptions {
	return func(obj *CSPCPatch) {
		obj.ResourcePatch = r
	}
}

// WithCSPCClient ...
func WithCSPCClient(c *Client) CSPCPatchOptions {
	return func(obj *CSPCPatch) {
		obj.Client = c
	}
}

// NewCSPCPatch ...
func NewCSPCPatch(opts ...CSPCPatchOptions) *CSPCPatch {
	obj := &CSPCPatch{}
	for _, o := range opts {
		o(obj)
	}
	return obj
}

// PreUpgrade ...
func (obj *CSPCPatch) PreUpgrade() error {
	err := isOperatorUpgraded("cspc-operator", obj.Namespace, obj.To, obj.KubeClientset)
	if err != nil {
		return err
	}
	err = obj.CSPC.PreChecks(obj.From, obj.To)
	return err
}

// Init initializes all the fields of the CSPCPatch
func (obj *CSPCPatch) Init() error {
	obj.Namespace = obj.OpenebsNamespace
	obj.CSPC = patch.NewCSPC(
		patch.WithCSPCClient(obj.OpenebsClientset),
	)
	err := obj.CSPC.Get(obj.Name, obj.Namespace)
	if err != nil {
		return err
	}
	err = getCSPCPatchData(obj)
	return err
}

func getCSPCPatchData(obj *CSPCPatch) error {
	newCSPC := obj.CSPC.Object.DeepCopy()
	err := transformCSPC(newCSPC, obj.ResourcePatch)
	if err != nil {
		return err
	}
	obj.CSPC.Data, err = GetPatchData(obj.CSPC.Object, newCSPC)
	return err
}

func transformCSPC(c *cstor.CStorPoolCluster, res *ResourcePatch) error {
	c.VersionDetails.Desired = res.To
	return nil
}

// CSPCUpgrade ...
func (obj *CSPCPatch) CSPCUpgrade() error {
	err := obj.CSPC.Patch(obj.From, obj.To)
	if err != nil {
		return err
	}
	return nil
}

// Upgrade execute the steps to upgrade CSPC
func (obj *CSPCPatch) Upgrade() error {
	err := obj.Init()
	if err != nil {
		return err
	}
	err = obj.PreUpgrade()
	if err != nil {
		return err
	}
	res := *obj.ResourcePatch
	cspiList, err := obj.Client.OpenebsClientset.CstorV1().
		CStorPoolInstances(obj.Namespace).List(context.TODO(),
		metav1.ListOptions{
			LabelSelector: "openebs.io/cstor-pool-cluster=" + obj.Name,
		},
	)
	if err != nil {
		return err
	}
	for _, cspiObj := range cspiList.Items {
		res.Name = cspiObj.Name
		dependant := NewCSPIPatch(
			WithCSPIResorcePatch(&res),
			WithCSPIClient(obj.Client),
		)
		err = dependant.Upgrade()
		if err != nil {
			utaskObj, uerr := obj.OpenebsClientset.OpenebsV1alpha1().
				UpgradeTasks(obj.OpenebsNamespace).
				Get(context.TODO(), "upgrade-cstor-cspi-"+cspiObj.Name, metav1.GetOptions{})
			if uerr != nil && isUpgradeTaskJob {
				return uerr
			}
			backoffLimit, uerr := getBackoffLimit(obj.OpenebsNamespace, obj.Client)
			if uerr != nil && isUpgradeTaskJob {
				return uerr
			}
			utaskObj.Status.Retries = utaskObj.Status.Retries + 1
			if utaskObj.Status.Retries == backoffLimit {
				utaskObj.Status.Phase = v1Alpha1API.UpgradeError
				utaskObj.Status.CompletedTime = metav1.Now()
			}
			_, uerr = obj.OpenebsClientset.OpenebsV1alpha1().UpgradeTasks(obj.OpenebsNamespace).
				Update(context.TODO(), utaskObj, metav1.UpdateOptions{})
			if uerr != nil && isUpgradeTaskJob {
				return uerr
			}
			return err
		}
		utaskObj, uerr := obj.OpenebsClientset.OpenebsV1alpha1().UpgradeTasks(obj.OpenebsNamespace).
			Get(context.TODO(), "upgrade-cstor-cspi-"+cspiObj.Name, metav1.GetOptions{})
		if uerr != nil && isUpgradeTaskJob {
			return uerr
		}
		utaskObj.Status.Phase = v1Alpha1API.UpgradeSuccess
		utaskObj.Status.CompletedTime = metav1.Now()
		_, uerr = obj.OpenebsClientset.OpenebsV1alpha1().UpgradeTasks(obj.OpenebsNamespace).
			Update(context.TODO(), utaskObj, metav1.UpdateOptions{})
		if uerr != nil && isUpgradeTaskJob {
			return uerr
		}
	}
	err = obj.CSPCUpgrade()
	if err != nil {
		return err
	}
	err = obj.verifyCSPCVersionReconcile()
	return err
}

func (obj *CSPCPatch) verifyCSPCVersionReconcile() error {
	// get the latest cspc object
	err := obj.CSPC.Get(obj.Name, obj.Namespace)
	if err != nil {
		return err
	}
	// waiting for the current version to be equal to desired version
	for obj.CSPC.Object.VersionDetails.Status.Current != obj.To {
		klog.Infof("Verifying the reconciliation of version for %s", obj.CSPC.Object.Name)
		// Sleep equal to the default sync time
		time.Sleep(10 * time.Second)
		err = obj.CSPC.Get(obj.Name, obj.Namespace)
		if err != nil {
			return err
		}
		if obj.CSPC.Object.VersionDetails.Status.Message != "" {
			klog.Errorf("failed to reconcile: %s", obj.CSPC.Object.VersionDetails.Status.Reason)
		}
	}
	return nil
}
