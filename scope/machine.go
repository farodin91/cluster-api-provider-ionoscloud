/*
Copyright 2024 IONOS Cloud.

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

package scope

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-ionoscloud/api/v1alpha1"
	"github.com/ionos-cloud/cluster-api-provider-ionoscloud/internal/util/ptr"
)

// Machine defines a basic machine context for primary use in IonosCloudMachineReconciler.
type Machine struct {
	client      client.Client
	patchHelper *patch.Helper

	Machine      *clusterv1.Machine
	IonosMachine *infrav1.IonosCloudMachine

	ClusterScope *Cluster
}

// MachineParams is a struct that contains the params used to create a new Machine through NewMachine.
type MachineParams struct {
	Client       client.Client
	Machine      *clusterv1.Machine
	ClusterScope *Cluster
	IonosMachine *infrav1.IonosCloudMachine
}

// NewMachine creates a new Machine using the provided params.
func NewMachine(params MachineParams) (*Machine, error) {
	if params.Client == nil {
		return nil, errors.New("machine scope params lack a client")
	}
	if params.Machine == nil {
		return nil, errors.New("machine scope params lack a Cluster API machine")
	}
	if params.IonosMachine == nil {
		return nil, errors.New("machine scope params lack a IONOS Cloud machine")
	}
	if params.ClusterScope == nil {
		return nil, errors.New("machine scope params need a IONOS Cloud cluster scope")
	}

	helper, err := patch.NewHelper(params.IonosMachine, params.Client)
	if err != nil {
		return nil, fmt.Errorf("failed to init patch helper: %w", err)
	}
	return &Machine{
		client:       params.Client,
		patchHelper:  helper,
		Machine:      params.Machine,
		ClusterScope: params.ClusterScope,
		IonosMachine: params.IonosMachine,
	}, nil
}

// GetBootstrapDataSecret returns the bootstrap data secret, which has been created by the
// Kubeadm provider.
func (m *Machine) GetBootstrapDataSecret(ctx context.Context, log logr.Logger) (*corev1.Secret, error) {
	name := ptr.Deref(m.Machine.Spec.Bootstrap.DataSecretName, "")
	if name == "" {
		return nil, errors.New("machine has no bootstrap data yet")
	}
	key := client.ObjectKey{
		Name:      name,
		Namespace: m.IonosMachine.Namespace,
	}

	log.WithName("GetBoostrapDataSecret").
		V(4).
		Info("searching for bootstrap data", "secret", key.String())

	var lookupSecret corev1.Secret
	if err := m.client.Get(ctx, key, &lookupSecret); err != nil {
		return nil, err
	}

	return &lookupSecret, nil
}

// DatacenterID returns the data center ID used by the IonosCloudMachine.
func (m *Machine) DatacenterID() string {
	return m.IonosMachine.Spec.DatacenterID
}

// SetProviderID sets the provider ID for the IonosCloudMachine.
func (m *Machine) SetProviderID(id string) {
	m.IonosMachine.Spec.ProviderID = ptr.To("ionos://" + id)
}

// CountMachines returns the number of existing IonosCloudMachines in the same namespace
// and with the same cluster label. With machineLabels, additional search labels can be provided.
func (m *Machine) CountMachines(ctx context.Context, machineLabels client.MatchingLabels) (int, error) {
	machines, err := m.ListMachines(ctx, machineLabels)
	return len(machines), err
}

// ListMachines is a convenience wrapper function for the Cluster.ListMachines function.
func (m *Machine) ListMachines(
	ctx context.Context,
	machineLabels client.MatchingLabels,
) ([]infrav1.IonosCloudMachine, error) {
	return m.ClusterScope.ListMachines(ctx, machineLabels)
}

// FindLatestMachine returns the latest IonosCloudMachine in the same namespace
// and with the same cluster label. If no machine was found, nil is returned.
//
// Only machines, that are different to the receiver machine, are considered.
// If the receiver machine is the only machine in the list, nil is returned.
func (m *Machine) FindLatestMachine(
	ctx context.Context,
	matchLabels client.MatchingLabels,
) (*infrav1.IonosCloudMachine, error) {
	machines, err := m.ClusterScope.ListMachines(ctx, matchLabels)
	if err != nil {
		return nil, err
	}

	if len(machines) <= 1 {
		return nil, nil
	}

	latestMachine := machines[0]
	for _, machine := range machines {
		if !machine.CreationTimestamp.Before(&latestMachine.CreationTimestamp) && machine.Name != m.IonosMachine.Name {
			latestMachine = machine
		}
	}
	if latestMachine.Name == m.IonosMachine.Name {
		return nil, nil
	}

	return &latestMachine, nil
}

// HasFailed checks if the IonosCloudMachine is in a failed state.
func (m *Machine) HasFailed() bool {
	status := m.IonosMachine.Status
	return status.FailureReason != nil || status.FailureMessage != nil
}

// PatchObject will apply all changes from the IonosMachine.
// It will also make sure to patch the status subresource.
func (m *Machine) PatchObject() error {
	conditions.SetSummary(m.IonosMachine,
		conditions.WithConditions(
			infrav1.MachineProvisionedCondition))

	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	// We don't accept and forward a context here. This is on purpose: Even if a reconciliation is
	// aborted, we want to make sure that the final patch is applied. Reusing the context from the reconciliation
	// would cause the patch to be aborted as well.
	return m.patchHelper.Patch(
		timeoutCtx,
		m.IonosMachine,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{
			clusterv1.ReadyCondition,
			infrav1.MachineProvisionedCondition,
		}})
}

// Finalize will make sure to apply a patch to the current IonosCloudMachine.
// It also implements a retry mechanism to increase the chance of success
// in case the patch operation was not successful.
func (m *Machine) Finalize() error {
	// NOTE(lubedacht) retry is only a way to reduce the failure chance,
	// but in general, the reconciliation logic must be resilient
	// to handle an outdated resource from that API server.
	shouldRetry := func(error) bool { return true }
	return retry.OnError(
		retry.DefaultBackoff,
		shouldRetry,
		m.PatchObject)
}
