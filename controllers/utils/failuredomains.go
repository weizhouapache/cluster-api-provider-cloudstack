/*
Copyright 2022 The Kubernetes Authors.

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

package utils

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	infrav1 "sigs.k8s.io/cluster-api-provider-cloudstack/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-cloudstack/pkg/cloud"

	"github.com/pkg/errors"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// CreateFailureDomain creates a specified CloudStackFailureDomain CRD owned by the ReconcilationSubject.
func (r *ReconciliationRunner) CreateFailureDomain(fdSpec infrav1.CloudStackFailureDomainSpec) error {
	metaHashName := infrav1.FailureDomainHashedMetaName(fdSpec.Name, r.CAPICluster.Name)
	csFD := &infrav1.CloudStackFailureDomain{
		ObjectMeta: r.NewChildObjectMeta(metaHashName),
		Spec:       fdSpec,
	}
	return errors.Wrap(r.K8sClient.Create(r.RequestCtx, csFD), "creating CloudStackFailureDomain")
}

// CreateFailureDomains creates a CloudStackFailureDomain CRD for each of the ReconcilationSubject's FailureDomains.
func (r *ReconciliationRunner) CreateFailureDomains(fdSpecs []infrav1.CloudStackFailureDomainSpec) CloudStackReconcilerMethod {
	return func() (ctrl.Result, error) {
		for _, fdSpec := range fdSpecs {
			if err := r.CreateFailureDomain(fdSpec); err != nil {
				if !strings.Contains(strings.ToLower(err.Error()), "already exists") {
					return reconcile.Result{}, errors.Wrap(err, "creating CloudStackFailureDomains")
				}
			}
		}
		return ctrl.Result{}, nil
	}
}

// GetFailureDomains gets CloudStackFailureDomains owned by a CloudStackCluster.
func (r *ReconciliationRunner) GetFailureDomains(fds *infrav1.CloudStackFailureDomainList) CloudStackReconcilerMethod {
	return func() (ctrl.Result, error) {
		capiClusterLabel := map[string]string{clusterv1.ClusterNameLabel: r.CSCluster.GetLabels()[clusterv1.ClusterNameLabel]}
		if err := r.K8sClient.List(
			r.RequestCtx,
			fds,
			client.InNamespace(r.Request.Namespace),
			client.MatchingLabels(capiClusterLabel),
		); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to list failure domains")
		}
		return ctrl.Result{}, nil
	}
}

// GetFailureDomainByName gets a single FailureDomain by name and requeues if it's not found.
func (r *ReconciliationRunner) GetFailureDomainByName(nameFunc func() string, fd *infrav1.CloudStackFailureDomain) CloudStackReconcilerMethod {
	return func() (ctrl.Result, error) {
		metaHashName := infrav1.FailureDomainHashedMetaName(nameFunc(), r.CAPICluster.Name)
		if err := r.K8sClient.Get(r.RequestCtx, client.ObjectKey{Namespace: r.Request.Namespace, Name: metaHashName}, fd); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get failure domain with name %s", nameFunc())
		}
		return ctrl.Result{}, nil
	}
}

// RemoveExtraneousFailureDomains deletes failure domains no longer listed under the CloudStackCluster's spec.
func (r *ReconciliationRunner) RemoveExtraneousFailureDomains(fds *infrav1.CloudStackFailureDomainList) CloudStackReconcilerMethod {
	return func() (ctrl.Result, error) {
		// Toss together a precense map.
		fdPresenceByName := map[string]bool{}
		for _, fdSpec := range r.CSCluster.Spec.FailureDomains {
			name := fdSpec.Name
			fdPresenceByName[name] = true
		}

		// Send a deletion request for each FailureDomain not speced for.
		for _, fd := range fds.Items {
			if _, present := fdPresenceByName[fd.Spec.Name]; !present {
				toDelete := fd
				r.Log.Info(fmt.Sprintf("Deleting extraneous failure domain: %s.", fd.Name))
				if err := r.K8sClient.Delete(r.RequestCtx, &toDelete); err != nil {
					return ctrl.Result{}, errors.Wrap(err, "failed to delete obsolete failure domain")
				}
			}
		}
		return ctrl.Result{}, nil
	}
}

// GetFailureDomainsAndRequeueIfMissing gets CloudStackFailureDomains owned by a CloudStackCluster and requeues if none are found.
func (r *ReconciliationRunner) GetFailureDomainsAndRequeueIfMissing(fds *infrav1.CloudStackFailureDomainList) CloudStackReconcilerMethod {
	return func() (ctrl.Result, error) {
		if res, err := r.GetFailureDomains(fds)(); r.ShouldReturn(res, err) {
			return res, err
		} else if len(fds.Items) < 1 {
			return r.RequeueWithMessage("no failure domains found, requeueing")
		}
		return ctrl.Result{}, nil
	}
}

type CloudClientExtension interface {
	RegisterExtension(*ReconciliationRunner) CloudClientExtension
	AsFailureDomainUser(*infrav1.CloudStackFailureDomainSpec) CloudStackReconcilerMethod
}

type CloudClientImplementation struct {
	CloudClientExtension
	*ReconciliationRunner
}

func (c *CloudClientImplementation) RegisterExtension(r *ReconciliationRunner) CloudClientExtension {
	c.ReconciliationRunner = r
	return c
}

// AsFailureDomainUser uses the credentials specified in the failure domain to set the ReconciliationSubject's CSUser client.
func (c *CloudClientImplementation) AsFailureDomainUser(fdSpec *infrav1.CloudStackFailureDomainSpec) CloudStackReconcilerMethod {
	return func() (ctrl.Result, error) {
		endpointCredentials := &corev1.Secret{}
		key := client.ObjectKey{Name: fdSpec.ACSEndpoint.Name, Namespace: fdSpec.ACSEndpoint.Namespace}
		if err := c.K8sClient.Get(c.RequestCtx, key, endpointCredentials); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "getting ACSEndpoint secret with ref: %v", fdSpec.ACSEndpoint)
		}

		clientConfig := &corev1.ConfigMap{}
		key = client.ObjectKey{Name: cloud.ClientConfigMapName, Namespace: cloud.ClientConfigMapNamespace}
		_ = c.K8sClient.Get(c.RequestCtx, key, clientConfig)

		var err error
		if c.CSClient, err = cloud.NewClientFromK8sSecret(endpointCredentials, clientConfig); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "parsing ACSEndpoint secret with ref: %v", fdSpec.ACSEndpoint)
		}

		if fdSpec.Account != "" { // Set r.CSUser CloudStack Client per Account and Domain.
			client, err := c.CSClient.NewClientInDomainAndAccount(fdSpec.Domain, fdSpec.Account)
			if err != nil {
				return ctrl.Result{}, err
			}
			c.CSUser = client
		} else { // Set r.CSUser CloudStack Client to r.CSClient since Account & Domain weren't provided.
			c.CSUser = c.CSClient
		}

		return ctrl.Result{}, nil
	}
}
